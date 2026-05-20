package symbolize

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

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
	sc := bufio.NewScanner(f)
	// /proc/kallsyms lines are short, but allocate generously so the
	// scanner never errors on long module-symbol names.
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		// Type filter: only addressable code symbols. Matches the
		// kinds the awk hack in resolve_user_addrs.py keeps for
		// userspace; same logic applies to kernel symbols.
		switch fields[1] {
		case "T", "t", "W", "w", "i":
		default:
			continue
		}
		addr, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil {
			continue
		}
		if addr != 0 {
			sawNZ = true
		}
		module := ""
		if len(fields) >= 4 {
			module = fields[3] // "[modname]"
		}
		addrs = append(addrs, addr)
		names = append(names, fields[2])
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
