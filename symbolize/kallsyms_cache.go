package symbolize

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Disk cache for the parsed /proc/kallsyms index. Motivated by
// bench-self iter 6: on lockdown hosts (Secure Boot,
// integrity-locked) every perf-agent invocation has to parse the
// 3M-line kallsyms file from scratch because blazesym's kernel
// source hits EPERM on /proc/kcore. The parse itself is fast
// (allocation-free per iter 5) but the kernel synthesizes the
// file via vsnprintf on each read syscall — that's the floor.
//
// Cache key: the kernel boot_id. Kernel symbol addresses change
// only across reboots (KASLR), so the boot_id is the right
// invalidation signal — drops the cache exactly when it would
// be wrong, never falsely keeps a stale copy.
//
// Format (little-endian):
//
//	header: magic u32 | version u32 | boot_id [16]byte | n_syms u32
//	per-symbol: addr u64 | name_len u16 | module_len u16 | name | module
//
// One symbol record averages ~50 bytes; a typical kernel's filtered
// kallsyms (~1.5M kept after the T/t/W/w/i filter) lands at ~70 MiB
// on disk. Small enough to ship in ~/.cache without ceremony.
const (
	kallsymsCacheMagic   uint32 = 0x4B414C53 // 'KALS'
	kallsymsCacheVersion uint32 = 1
	kallsymsHeaderSize          = 4 + 4 + 16 + 4
)

// errKallsymsCacheStale signals the cache was produced under a
// different kernel boot_id. Caller must reparse /proc/kallsyms.
var errKallsymsCacheStale = errors.New("kallsyms: cache stale (kernel rebooted)")

// errKallsymsCacheCorrupt signals an unreadable or wrong-magic file.
// Non-fatal: caller falls back to a fresh parse and the next
// successful parse will overwrite the bad file.
var errKallsymsCacheCorrupt = errors.New("kallsyms: cache corrupt")

// Indirection seams so tests can pin the cache path + boot_id.
// Production: cachePathFn = kallsymsDefaultCachePath,
//             readBootIDFn = readBootID.
var (
	cachePathFn  = kallsymsDefaultCachePath
	readBootIDFn = readBootID
)

// kallsymsDefaultCachePath honors $XDG_CACHE_HOME and falls back to
// ~/.cache; final fallback is /tmp so the cache works even for
// daemons with no $HOME set.
func kallsymsDefaultCachePath() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".cache")
		} else {
			base = "/tmp"
		}
	}
	return filepath.Join(base, "perf-agent", "kallsyms.cache")
}

// readBootID parses /proc/sys/kernel/random/boot_id (a UUID like
// "12345678-1234-1234-1234-1234567890ab") into raw bytes.
func readBootID() ([16]byte, error) {
	var out [16]byte
	body, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return out, err
	}
	text := strings.TrimSpace(string(body))
	text = strings.ReplaceAll(text, "-", "")
	if len(text) != 32 {
		return out, fmt.Errorf("kallsyms: unexpected boot_id length %d", len(text))
	}
	b, err := hex.DecodeString(text)
	if err != nil {
		return out, fmt.Errorf("kallsyms: parse boot_id: %w", err)
	}
	copy(out[:], b)
	return out, nil
}

// loadCachedKallsyms tries to materialize a kallsymsSymbolizer from
// the on-disk cache. Returns:
//   - (s, nil) on success
//   - (nil, errKallsymsCacheStale) when boot_id changed
//   - (nil, errKallsymsCacheCorrupt) on magic / size mismatch
//   - (nil, fs error) on missing file / read failure
//
// The caller treats any error as "fall back to a fresh parse".
func loadCachedKallsyms() (*kallsymsSymbolizer, error) {
	path := cachePathFn()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < kallsymsHeaderSize {
		return nil, errKallsymsCacheCorrupt
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	version := binary.LittleEndian.Uint32(data[4:8])
	if magic != kallsymsCacheMagic || version != kallsymsCacheVersion {
		return nil, errKallsymsCacheCorrupt
	}
	var cachedBoot [16]byte
	copy(cachedBoot[:], data[8:24])
	currentBoot, err := readBootIDFn()
	if err != nil {
		return nil, fmt.Errorf("kallsyms: read boot_id: %w", err)
	}
	if cachedBoot != currentBoot {
		return nil, errKallsymsCacheStale
	}
	nSyms := int(binary.LittleEndian.Uint32(data[24:28]))

	addrs := make([]uint64, nSyms)
	names := make([]string, nSyms)
	modules := make([]string, nSyms)
	modIntern := make(map[string]string, 64)
	off := kallsymsHeaderSize
	for i := range nSyms {
		if off+12 > len(data) {
			return nil, errKallsymsCacheCorrupt
		}
		addrs[i] = binary.LittleEndian.Uint64(data[off : off+8])
		nameLen := int(binary.LittleEndian.Uint16(data[off+8 : off+10]))
		modLen := int(binary.LittleEndian.Uint16(data[off+10 : off+12]))
		off += 12
		if off+nameLen+modLen > len(data) {
			return nil, errKallsymsCacheCorrupt
		}
		names[i] = string(data[off : off+nameLen])
		off += nameLen
		if modLen > 0 {
			s := string(data[off : off+modLen])
			if interned, ok := modIntern[s]; ok {
				modules[i] = interned
			} else {
				modIntern[s] = s
				modules[i] = s
			}
			off += modLen
		}
	}
	return &kallsymsSymbolizer{
		addrs:   addrs,
		names:   names,
		modules: modules,
	}, nil
}

// writeKallsymsCache serializes s to the cache path. Best-effort:
// errors are returned but callers MUST treat write failure as
// non-fatal (the next run just re-parses). Writes go to a temp
// file and rename for atomicity — partial writes never expose a
// corrupt file to concurrent readers.
func writeKallsymsCache(s *kallsymsSymbolizer) error {
	bootID, err := readBootIDFn()
	if err != nil {
		return err
	}
	path := cachePathFn()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	// On any error after this point: close and unlink the temp.
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	if err := writeKallsymsCacheTo(bw, s, bootID); err != nil {
		cleanup()
		return err
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// writeKallsymsCacheTo encodes the header + symbol stream onto w.
// Split out from writeKallsymsCache so tests can verify the
// format without touching disk.
func writeKallsymsCacheTo(w io.Writer, s *kallsymsSymbolizer, bootID [16]byte) error {
	var hdr [kallsymsHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[0:4], kallsymsCacheMagic)
	binary.LittleEndian.PutUint32(hdr[4:8], kallsymsCacheVersion)
	copy(hdr[8:24], bootID[:])
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(len(s.addrs)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	var symHdr [12]byte
	for i, addr := range s.addrs {
		name := s.names[i]
		mod := s.modules[i]
		// Name / module lengths must fit in uint16. Truncate
		// pathologically long names rather than fail the whole
		// write (no real-world kernel symbol exceeds 65 KiB).
		nameLen := len(name)
		if nameLen > 0xFFFF {
			nameLen = 0xFFFF
		}
		modLen := len(mod)
		if modLen > 0xFFFF {
			modLen = 0xFFFF
		}
		binary.LittleEndian.PutUint64(symHdr[0:8], addr)
		binary.LittleEndian.PutUint16(symHdr[8:10], uint16(nameLen))
		binary.LittleEndian.PutUint16(symHdr[10:12], uint16(modLen))
		if _, err := w.Write(symHdr[:]); err != nil {
			return err
		}
		if _, err := io.WriteString(w, name[:nameLen]); err != nil {
			return err
		}
		if modLen > 0 {
			if _, err := io.WriteString(w, mod[:modLen]); err != nil {
				return err
			}
		}
	}
	return nil
}
