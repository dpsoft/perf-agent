package symbolize

import (
	"bufio"
	"fmt"
	"os"
	"sort"
)

// parseKallsymsLine extracts (addr, type, name, module) from one
// /proc/kallsyms line without allocating. Returns ok=false on
// malformed lines.
//
// Format:  "<16-hex-addr> <type-byte> <name>[ \t]+[module]"
// Example: "ffffffff80c6a050 T __x64_sys_open"
// Example: "ffffffff8e2b0010 T kvm_init  [kvm]"
//
// Name and module are slices into the input buffer; the caller is
// responsible for copying them out before the buffer is reused.
func parseKallsymsLine(line []byte) (addr uint64, typ byte, name, module []byte, ok bool) {
	i := 0
	// hex addr — accept any run of hex digits, stop at first non-hex
	for i < len(line) {
		c := line[i]
		v := uint64(0)
		switch {
		case c >= '0' && c <= '9':
			v = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			v = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = uint64(c-'A') + 10
		default:
			goto endAddr
		}
		addr = addr<<4 | v
		i++
	}
endAddr:
	if i == 0 {
		return 0, 0, nil, nil, false
	}
	if i >= len(line) || line[i] != ' ' {
		return 0, 0, nil, nil, false
	}
	i++
	if i >= len(line) {
		return 0, 0, nil, nil, false
	}
	typ = line[i]
	i++
	if i >= len(line) || line[i] != ' ' {
		return 0, 0, nil, nil, false
	}
	i++
	nameStart := i
	for i < len(line) && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	name = line[nameStart:i]
	// optional module: whitespace then "[modname]"
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i < len(line) {
		module = line[i:]
	}
	return addr, typ, name, module, true
}

// kallsymsSymbolizer resolves kernel addresses by binary search
// against a snapshot of /proc/kallsyms. Used as the lockdown-safe
// fallback when blazesym fails with permission-denied (e.g., on
// Secure-Boot hosts where blazesym's kernel source tries /proc/kcore
// and gets EACCES, even with vmlinux="").
//
// Resolution is name + offset only — no inline expansion, no
// file:line. /proc/kallsyms doesn't carry DWARF, and that's
// acceptable: operators get readable flame graphs, which is the
// load-bearing property.
type kallsymsSymbolizer struct {
	addrs   []uint64 // sorted ascending; parallel to names/modules
	names   []string
	modules []string // "" for vmlinux, "[xfs]" etc. for modules
}

// newKallsymsSymbolizer parses /proc/kallsyms into a sorted index.
// Returns ErrKernelSymbolsUnavailable when the file is unreadable or
// returns zero addresses (kptr-restricted).
//
// Tuned across two bench-self iterations:
//   - iter 3: wrap the file in a 256 KiB bufio.Reader so each
//     read() pulls many lines at once (the kernel synthesizes
//     kallsyms via vsnprintf per read; small reads forced
//     repeated trips through that path).
//   - iter 5: byte-level allocation-free line parser, replacing
//     strings.Fields + strconv.ParseUint. The previous version
//     was the top allocation source in perf-agent's user-side
//     pprof: 3M kallsyms lines × strings.Fields × ParseUint =
//     ~9M+ allocations, triggering noticeable GC pressure
//     (sweepone, tryDeferToSpanScan, mallocgc).
//
// One string allocation per KEPT symbol (the name; copies out of
// the read buffer so the buffer can be reused). Modules deduped
// via an intern map: a typical kernel has tens of modules but
// millions of module symbols.
func newKallsymsSymbolizer() (*kallsymsSymbolizer, error) {
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		return nil, fmt.Errorf("kallsyms: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	var (
		addrs   []uint64
		names   []string
		modules []string
		sawNZ   bool
	)
	moduleIntern := make(map[string]string, 64)
	br := bufio.NewReaderSize(f, 256*1024)
	sc := bufio.NewScanner(br)
	// Token buffer: 4 KiB initial (typical line), 1 MiB max for
	// pathologically long module-symbol names.
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		addr, typ, nameBytes, modBytes, ok := parseKallsymsLine(line)
		if !ok {
			continue
		}
		// Type filter: only addressable code symbols. Matches the
		// kinds the awk hack in resolve_user_addrs.py keeps for
		// userspace; same logic applies to kernel symbols.
		switch typ {
		case 'T', 't', 'W', 'w', 'i':
		default:
			continue
		}
		if addr != 0 {
			sawNZ = true
		}
		module := ""
		if len(modBytes) > 0 {
			s := string(modBytes)
			if interned, ok := moduleIntern[s]; ok {
				module = interned
			} else {
				moduleIntern[s] = s
				module = s
			}
		}
		addrs = append(addrs, addr)
		names = append(names, string(nameBytes)) // one alloc per kept name
		modules = append(modules, module)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("kallsyms: scan: %w", err)
	}
	if len(addrs) == 0 || !sawNZ {
		return nil, ErrKernelSymbolsUnavailable
	}

	// /proc/kallsyms is already sorted by address on every supported
	// kernel, but the contract isn't formally documented anywhere we
	// can lean on — sort defensively.
	idx := make([]int, len(addrs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return addrs[idx[i]] < addrs[idx[j]] })
	sortedAddrs := make([]uint64, len(addrs))
	sortedNames := make([]string, len(addrs))
	sortedModules := make([]string, len(addrs))
	for i, j := range idx {
		sortedAddrs[i] = addrs[j]
		sortedNames[i] = names[j]
		sortedModules[i] = modules[j]
	}
	return &kallsymsSymbolizer{
		addrs:   sortedAddrs,
		names:   sortedNames,
		modules: sortedModules,
	}, nil
}

// maxKallsymsOffset bounds how far past a symbol an IP may land
// before we treat the resolution as bogus. The awk hack uses 64 KiB
// for userspace; kernel functions tend to be smaller, but conservatively
// 64 KiB rejects only obvious mis-attributions (gaps between subsystem
// regions in vmlinux).
const maxKallsymsOffset = 0x10000

// Resolve returns one Frame per IP via at-or-below binary search.
// IPs that map to a symbol within maxKallsymsOffset get the symbol
// name + module + offset. IPs outside that window get a raw-hex Name
// and Reason=FailureUnknownAddress, matching the rawKernelAddrFrames
// posture so kernel context still survives into the pprof.
func (k *kallsymsSymbolizer) Resolve(ips []uint64) []Frame {
	out := make([]Frame, len(ips))
	for i, ip := range ips {
		// sort.Search returns the lowest j with addrs[j] > ip;
		// the matching symbol is at j-1 (largest addr <= ip).
		j := sort.Search(len(k.addrs), func(j int) bool { return k.addrs[j] > ip })
		if j == 0 {
			out[i] = Frame{
				Address: ip,
				Name:    fmt.Sprintf("0x%x", ip),
				Module:  "[kernel.kallsyms]",
				Reason:  FailureUnknownAddress,
			}
			continue
		}
		symIdx := j - 1
		symAddr := k.addrs[symIdx]
		offset := ip - symAddr
		if offset > maxKallsymsOffset {
			out[i] = Frame{
				Address: ip,
				Name:    fmt.Sprintf("0x%x", ip),
				Module:  "[kernel.kallsyms]",
				Reason:  FailureUnknownAddress,
			}
			continue
		}
		module := k.modules[symIdx]
		if module == "" {
			module = "[kernel.kallsyms]"
		}
		out[i] = Frame{
			Address: ip,
			Name:    k.names[symIdx],
			Module:  module,
			Offset:  offset,
		}
	}
	return out
}
