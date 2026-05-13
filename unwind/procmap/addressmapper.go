package procmap

import (
	"debug/elf"
	"fmt"
	"os"
	"slices"
)

// AddressMapper is a port of pfelf.AddressMapper from
// github.com/open-telemetry/opentelemetry-ebpf-profiler
// (libpf/pfelf/addressmapper.go) — Apache-2.0, used per §4 of the license.
// Original copyright: Elasticsearch B.V. / OpenTelemetry Authors.
//
// Maps a file offset within an ELF to the virtual address that offset
// would have in the running image, following the kernel's mmap alignment
// of PT_LOAD p_offset to the page boundary. Used to convert file-relative
// addresses (from /proc/<pid>/maps) into ELF-relative virtual addresses
// for symbolization.
type AddressMapper struct {
	pageSize uint64
	loads    []ptLoad
}

type ptLoad struct {
	Off    uint64 // p_offset (page-aligned by NewAddressMapper)
	Vaddr  uint64 // p_vaddr
	Filesz uint64 // p_filesz
}

// NewAddressMapper reads PHDRs from the ELF at path and returns a mapper
// for its executable PT_LOAD segments. Callers should choose a path that's
// readable from the agent's namespace — typically mapping.OpenablePath()
// which prefers /proc/<pid>/map_files/<va>-<va>.
func NewAddressMapper(path string) (*AddressMapper, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("address mapper: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	pageSize := uint64(os.Getpagesize())
	var loads []ptLoad
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Flags&elf.PF_X == 0 {
			continue // only executable segments matter for symbolization
		}
		// OTel's correctness fix: page-align p_offset DOWN to mirror
		// kernel mmap alignment. Without this, offsets near segment
		// starts get misattributed.
		aligned := p.Off &^ (pageSize - 1)
		loads = append(loads, ptLoad{
			Off:    aligned,
			Vaddr:  p.Vaddr,
			Filesz: p.Filesz + (p.Off - aligned),
		})
	}
	slices.SortFunc(loads, func(a, b ptLoad) int {
		if a.Off < b.Off {
			return -1
		}
		if a.Off > b.Off {
			return 1
		}
		return 0
	})
	return &AddressMapper{pageSize: pageSize, loads: loads}, nil
}

// FileOffsetToVirtualAddress maps a file offset to its ELF virtual address.
// Returns (0, false) if the offset is outside every executable PT_LOAD.
func (m *AddressMapper) FileOffsetToVirtualAddress(off uint64) (uint64, bool) {
	for _, l := range m.loads {
		if off >= l.Off && off < l.Off+l.Filesz {
			return l.Vaddr + (off - l.Off), true
		}
	}
	return 0, false
}
