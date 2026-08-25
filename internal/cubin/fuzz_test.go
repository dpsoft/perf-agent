package cubin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var fixtureNames = []string{
	"single_lineinfo",
	"single_nolineinfo",
	"two_kernels_lineinfo",
	"unrolled_lineinfo",
}

// FuzzParse feeds Parse truncated and byte-flipped cubins. Cubin bytes reach
// the agent from a profiled process, so Parse is a trust boundary: it must
// return an error, never panic, and never allocate out of proportion to its
// input.
func FuzzParse(f *testing.F) {
	for _, name := range fixtureNames {
		b, err := os.ReadFile(filepath.Join("testdata", name+".cubin"))
		if err != nil {
			f.Fatalf("fixture %s: %v", name, err)
		}
		f.Add(b)

		// Truncations, including inside the ELF header, the section headers,
		// the symbol table and .debug_line.
		for _, frac := range []int{1, 2, 3, 4, 8, 16, 64, 256} {
			f.Add(b[:len(b)/frac])
		}
		f.Add(b[:len(b)-1])

		// Byte flips at structurally interesting places: e_machine, e_shoff,
		// e_shnum, and spread through the section data.
		for _, off := range []int{0, 4, 16, 18, 32, 40, 58, 60, 62} {
			if off < len(b) {
				m := append([]byte(nil), b...)
				m[off] ^= 0xff
				f.Add(m)
			}
		}
		for off := 0; off < len(b); off += 337 {
			m := append([]byte(nil), b...)
			m[off] ^= 0x01
			f.Add(m)
		}
	}
	f.Add([]byte(nil))
	f.Add([]byte{0x7f, 'E', 'L', 'F'})

	f.Fuzz(func(t *testing.T, in []byte) {
		c, err := Parse(in)
		if errors.Is(err, ErrPanic) {
			t.Fatalf("Parse panicked on %d bytes: %v", len(in), err)
		}
		if err != nil {
			if c != nil {
				t.Fatalf("Parse returned both an error and a cubin")
			}
			return
		}
		if c == nil {
			t.Fatal("Parse returned neither an error nor a cubin")
		}

		// Allocation must stay proportional to the input. Functions come from
		// the symbol table and line-table rows are produced by a line program
		// whose smallest row-emitting opcode is one byte, so neither can
		// outnumber the input bytes.
		fns := c.Functions()
		if len(fns) > maxFunctions {
			t.Fatalf("%d functions, over the %d cap", len(fns), maxFunctions)
		}
		if len(fns) > len(in) {
			t.Fatalf("%d functions from %d bytes", len(fns), len(in))
		}
		total := 0
		for _, rows := range c.rows {
			total += len(rows)
		}
		if total > len(in) {
			t.Fatalf("%d line-table rows from %d bytes", total, len(in))
		}

		// A row can only be bound to a function of this cubin, and only within
		// that function's own address range.
		byName := map[string]Function{}
		for _, fn := range fns {
			byName[fn.Name] = fn
		}
		for name, rows := range c.rows {
			fn, ok := byName[name]
			if !ok {
				t.Fatalf("rows bound to unknown function %q", name)
			}
			for _, r := range rows {
				if fn.Size > 0 && r.pcOffset > fn.Size {
					t.Fatalf("%s: row at %#x is past the function's %d bytes", name, r.pcOffset, fn.Size)
				}
			}
		}

		// Resolve must not panic for any offset, and must never answer when
		// there is no line table at all.
		for _, fn := range fns {
			for _, pc := range []uint64{0, 1, 15, 16, fn.Size, fn.Size + 1, 1 << 32, ^uint64(0)} {
				file, line, ok := c.Resolve(fn.Name, pc)
				if ok && !c.HasLineInfo() {
					t.Fatalf("%s resolved pcOffset %#x with no line table", fn.Name, pc)
				}
				if ok && !fn.HasLineInfo {
					t.Fatalf("%s resolved pcOffset %#x though it has no bound sequence", fn.Name, pc)
				}
				if !ok && (file != "" || line != 0) {
					t.Fatalf("%s: Resolve returned data alongside ok=false", fn.Name)
				}
			}
		}
		if _, _, ok := c.Resolve("\x00no-such-function", 0); ok {
			t.Fatal("resolved an unknown function")
		}
		// A damaged line table is never reported as an absent one.
		if c.LineInfoErr() != nil && !c.HasLineInfo() {
			t.Fatal("LineInfoErr is set though there is no .debug_line")
		}
	})
}
