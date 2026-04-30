package elfsym

import (
	"debug/elf"
	"errors"
	"fmt"
)

// FirstLoadVaddr returns the p_vaddr of the PT_LOAD segment that covers file
// offset 0 (the segment with the ELF header). Combined with the start address
// of the matching /proc/<pid>/maps entry, this gives the load bias:
//
//	load_bias = mapping_start - FirstLoadVaddr(path)
//	abs_addr  = load_bias + sym.Value
//
// For typical PIE shared libraries / PIE executables (p_vaddr == 0),
// load_bias collapses to mapping_start and the formula matches the
// long-standing convention. For non-PIE ET_EXEC binaries — Ubuntu
// /usr/bin/python3.12 has p_vaddr = 0x400000 and ships absolute symbol
// values — load_bias correctly resolves to 0, leaving sym.Value untouched.
// Without this correction, doubling mapping_start onto an already-absolute
// symbol drops RIP into garbage on remote call.
func FirstLoadVaddr(path string) (uint64, error) {
	f, err := elf.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open ELF %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Off != 0 {
			continue
		}
		return p.Vaddr, nil
	}
	return 0, errors.New("no PT_LOAD segment with file offset 0")
}

// ResolveSymbols opens the ELF file at path and resolves each symbol name in
// names to its file-offset value (the symbol's st_value). Returned map only
// contains entries for symbols that were found; missing symbols are silently
// absent. The caller adds the runtime load base to each value to compute the
// remote process's address.
//
// .dynsym is searched first (the dynamic symbol table is always present in
// shared libraries and required at runtime). If a name is not found there
// AND .symtab is present (i.e. binary not stripped), .symtab is searched as a
// fallback. This matters for some Python distributions that intentionally
// strip non-API symbols from .dynsym but leave .symtab intact.
//
// Returns os.ErrNotExist (wrapped) if path does not exist; other errors are
// wrapped with context.
func ResolveSymbols(path string, names []string) (map[string]uint64, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open ELF %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	out := make(map[string]uint64, len(names))
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}

	// Try .dynsym first.
	if dynsyms, derr := f.DynamicSymbols(); derr == nil {
		for _, sym := range dynsyms {
			if _, needed := want[sym.Name]; needed && sym.Value != 0 {
				out[sym.Name] = sym.Value
			}
		}
	}

	// Fall back to .symtab for any names still unresolved.
	stillMissing := false
	for _, n := range names {
		if _, ok := out[n]; !ok {
			stillMissing = true
			break
		}
	}
	if stillMissing {
		if syms, serr := f.Symbols(); serr == nil {
			for _, sym := range syms {
				if _, needed := want[sym.Name]; needed {
					if _, already := out[sym.Name]; already {
						continue
					}
					if sym.Value != 0 {
						out[sym.Name] = sym.Value
					}
				}
			}
		}
	}

	return out, nil
}
