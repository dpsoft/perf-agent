package elfsym

import (
	"debug/elf"
	"fmt"
)

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
	defer f.Close()

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
