package procmap

import (
	"cmp"
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

// ptLoad mirrors OTel's addressMapperPHDR: stores the raw PHDR fields
// (p_offset, p_vaddr, p_filesz) exactly as the ELF declares them. Page
// alignment is applied lazily inside FileOffsetToVirtualAddress so the
// lookup math works for both the un-aligned p_offset itself and any
// offset down to its page-aligned floor.
type ptLoad struct {
	Off    uint64 // p_offset (raw)
	Vaddr  uint64 // p_vaddr  (raw)
	Filesz uint64 // p_filesz (raw)
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
		// Store raw PHDR fields — alignment is handled at lookup time.
		loads = append(loads, ptLoad{
			Off:    p.Off,
			Vaddr:  p.Vaddr,
			Filesz: p.Filesz,
		})
	}
	slices.SortFunc(loads, func(a, b ptLoad) int {
		return cmp.Compare(a.Off, b.Off)
	})
	return &AddressMapper{pageSize: pageSize, loads: loads}, nil
}

// FileOffsetToVirtualAddress maps a file offset to its ELF virtual address.
// Returns (0, false) if the offset is outside every executable PT_LOAD.
//
// The accepted range is [alignedOffset, p.offset+p.filesz) where
// alignedOffset = p.offset &^ (pageSize-1). The asymmetric bounds mirror
// what the kernel does at mmap time: it rounds p_offset DOWN to a page
// boundary when creating the mapping, so file offsets in the pre-segment
// padding [alignedOffset, p.offset) are part of the live mapping even
// though they precede the declared start. The upper bound stays at the
// declared end so we don't claim bytes past p_filesz.
//
// The math vaddr + (off - p.offset) (equivalent to vaddr - (p.offset - off))
// reproduces the correct virtual address for every offset in that range:
//   - off == p.offset           → vaddr (un-aligned start maps to un-aligned VA)
//   - off == alignedOffset      → vaddr - (p.offset - alignedOffset)
//   - off in (p.offset, end)    → vaddr + positive delta
//
// Note: for off < p.Off the subexpression (off - p.Off) wraps under
// uint64 arithmetic; the wrap cancels when added to p.Vaddr, leaving
// the correct VA modulo 2^64 (which is what an address is).
func (m *AddressMapper) FileOffsetToVirtualAddress(off uint64) (uint64, bool) {
	pageMask := m.pageSize - 1
	for _, p := range m.loads {
		alignedOffset := p.Off &^ pageMask
		if off >= alignedOffset && off < p.Off+p.Filesz {
			return p.Vaddr + (off - p.Off), true
		}
	}
	return 0, false
}
