package symbolize

import (
	"bytes"
	"testing"
)

// TestParseKallsymsLine_Plain covers the no-module form.
func TestParseKallsymsLine_Plain(t *testing.T) {
	line := []byte("ffffffff80c6a050 T __x64_sys_open")
	addr, typ, name, module, ok := parseKallsymsLine(line)
	if !ok {
		t.Fatalf("parse failed")
	}
	if addr != 0xffffffff80c6a050 {
		t.Errorf("addr = %#x", addr)
	}
	if typ != 'T' {
		t.Errorf("typ = %q", typ)
	}
	if !bytes.Equal(name, []byte("__x64_sys_open")) {
		t.Errorf("name = %q", name)
	}
	if len(module) != 0 {
		t.Errorf("module = %q, want empty", module)
	}
}

// TestParseKallsymsLine_WithModule covers the "[modname]" suffix.
func TestParseKallsymsLine_WithModule(t *testing.T) {
	line := []byte("ffffffffc0001000 t kvm_vcpu_ioctl	[kvm]")
	addr, typ, name, module, ok := parseKallsymsLine(line)
	if !ok {
		t.Fatalf("parse failed")
	}
	if addr != 0xffffffffc0001000 {
		t.Errorf("addr = %#x", addr)
	}
	if typ != 't' {
		t.Errorf("typ = %q", typ)
	}
	if !bytes.Equal(name, []byte("kvm_vcpu_ioctl")) {
		t.Errorf("name = %q", name)
	}
	if !bytes.Equal(module, []byte("[kvm]")) {
		t.Errorf("module = %q", module)
	}
}

// TestParseKallsymsLine_LongAddrAccepted: kallsyms emits 16-hex-digit
// uppercase too on some kernels; parser must not assume case.
func TestParseKallsymsLine_LongAddrAccepted(t *testing.T) {
	line := []byte("FFFFFFFF80C6A050 T some_sym")
	addr, _, _, _, ok := parseKallsymsLine(line)
	if !ok {
		t.Fatalf("parse failed")
	}
	if addr != 0xFFFFFFFF80C6A050 {
		t.Errorf("addr = %#x", addr)
	}
}

// TestParseKallsymsLine_Empty / malformed return ok=false.
func TestParseKallsymsLine_Empty(t *testing.T) {
	for _, in := range [][]byte{
		nil,
		[]byte(""),
		[]byte("not_a_hex_addr"),
		[]byte("ffffffff80c6a050"), // missing type + name
		[]byte("ffffffff80c6a050 T"), // missing name
	} {
		if _, _, _, _, ok := parseKallsymsLine(in); ok {
			t.Errorf("parse %q unexpectedly succeeded", in)
		}
	}
}

// BenchmarkParseKallsymsLine measures the per-line cost so future
// kallsyms-parser changes can show their improvement (or regression).
func BenchmarkParseKallsymsLine(b *testing.B) {
	line := []byte("ffffffffc0001234 T kvm_vcpu_ioctl	[kvm]")
	for b.Loop() {
		_, _, _, _, _ = parseKallsymsLine(line)
	}
}
