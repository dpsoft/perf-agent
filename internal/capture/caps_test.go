package capture

import (
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fileCaps builds a security.capability value: the magic word, then
// (permitted, inheritable) for capabilities 0..31 and 32..63, then rootid
// for revision 3.
func fileCaps(magic uint32, permitted, inheritable uint64, rootid ...uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, magic)
	b = binary.LittleEndian.AppendUint32(b, uint32(permitted))
	b = binary.LittleEndian.AppendUint32(b, uint32(inheritable))
	b = binary.LittleEndian.AppendUint32(b, uint32(permitted>>32))
	b = binary.LittleEndian.AppendUint32(b, uint32(inheritable>>32))
	for _, r := range rootid {
		b = binary.LittleEndian.AppendUint32(b, r)
	}
	return b
}

func set(caps ...Capability) CapSet {
	var s CapSet
	for _, c := range caps {
		s |= 1 << c
	}
	return s
}

var required = set(CapBPF, CapPerfmon, CapSysPtrace, CapCheckpointRestore)

const (
	rev2      = 0x02000000
	rev3      = 0x03000000
	effective = 0x000001
)

func TestParseFileCapsAcceptsV2AndV3RootID0(t *testing.T) {
	// The exact bytes `setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep` writes.
	v2 := []byte{
		0x01, 0x00, 0x00, 0x02, // revision 2, effective
		0x00, 0x00, 0x08, 0x00, // permitted 0..31: cap_sys_ptrace (19)
		0x00, 0x00, 0x00, 0x00, // inheritable 0..31
		0xc0, 0x01, 0x00, 0x00, // permitted 32..63: cap_perfmon (38), cap_bpf (39), cap_checkpoint_restore (40)
		0x00, 0x00, 0x00, 0x00, // inheritable 32..63
	}
	got, err := ParseFileCaps(v2)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if got != required {
		t.Fatalf("v2 = %#x, want %#x", got, required)
	}
	got3, err := ParseFileCaps(fileCaps(rev3|effective, uint64(required), 0, 0))
	if err != nil {
		t.Fatalf("v3 rootid 0: %v", err)
	}
	if got3 != got {
		t.Fatalf("v3 = %#x, want the v2 set %#x", got3, got)
	}
	want := []string{"cap_sys_ptrace", "cap_perfmon", "cap_bpf", "cap_checkpoint_restore"}
	if names := got.Names(); !slices.Equal(names, want) {
		t.Fatalf("Names() = %q, want %q", names, want)
	}
	for _, c := range []Capability{CapBPF, CapPerfmon, CapSysPtrace, CapCheckpointRestore} {
		if !got.Has(c) {
			t.Errorf("Has(%d) = false", c)
		}
	}
	if got.Has(CapSyslog) || got.Has(CapSysAdmin) {
		t.Error("Has reports a capability that is not in the set")
	}
}

func TestParseFileCapsRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
		want string
	}{
		{"v3 rootid 1000", fileCaps(rev3|effective, uint64(required), 0, 1000), "rootid 1000"},
		{"no effective", fileCaps(rev2, uint64(required), 0), "effective"},
		{"inheritable", fileCaps(rev2|effective, uint64(required), uint64(set(CapSysPtrace))), "inheritable"},
		{"19 bytes", fileCaps(rev2|effective, uint64(required), 0)[:19], "got 19"},
		{"24-byte v2", fileCaps(rev2|effective, uint64(required), 0, 0), "revision 2 is 20 bytes, got 24"},
		{"bad magic", fileCaps(0x01000000|effective, uint64(required), 0), "revision"},
		{"unknown flags", fileCaps(rev2|effective|0x10, uint64(required), 0), "flags"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFileCaps(tc.b)
			if err == nil {
				t.Fatalf("accepted as %#x", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestCheckCapsPolicy(t *testing.T) {
	t.Run("missing cap_checkpoint_restore", func(t *testing.T) {
		_, err := checkCaps(set(CapBPF, CapPerfmon, CapSysPtrace, CapSyslog))
		e := asError(t, err, CodeDisabled)
		if !strings.Contains(e.Message, "cap_checkpoint_restore") {
			t.Errorf("message %q does not name cap_checkpoint_restore", e.Message)
		}
		if strings.Contains(e.Message, "cap_bpf") {
			t.Errorf("message %q names a capability that is present", e.Message)
		}
		if !strings.Contains(e.Next, SetcapLine) {
			t.Errorf("next %q does not quote %q", e.Next, SetcapLine)
		}
		if strings.Contains(e.Error(), "run as root") {
			t.Errorf("error %q advises root", e)
		}
	})
	t.Run("cap_dac_override refused by name", func(t *testing.T) {
		_, err := checkCaps(required | set(CapDACOverride))
		e := asError(t, err, CodeDisabled)
		if !strings.Contains(e.Message, "cap_dac_override") {
			t.Errorf("message %q does not name cap_dac_override", e.Message)
		}
		if !strings.Contains(e.Next, SetcapLine) {
			t.Errorf("next %q does not quote %q", e.Next, SetcapLine)
		}
	})
	t.Run("an unknown capability refused by number", func(t *testing.T) {
		_, err := checkCaps(required | 1<<50)
		e := asError(t, err, CodeDisabled)
		if !strings.Contains(e.Message, "cap_50") {
			t.Errorf("message %q does not name cap_50", e.Message)
		}
	})
	for _, extra := range []Capability{CapSyslog, CapSysAdmin} {
		w, err := checkCaps(required | set(extra))
		if err != nil {
			t.Errorf("+%v refused: %v", set(extra).Names(), err)
		}
		if len(w) != 0 {
			t.Errorf("+%v warnings %q, want none", set(extra).Names(), w)
		}
	}
	w, err := checkCaps(required | set(CapSyslog, CapDACReadSearch))
	if err != nil {
		t.Fatalf("+cap_dac_read_search refused: %v", err)
	}
	if len(w) != 1 || !strings.Contains(w[0], "cap_dac_read_search") {
		t.Fatalf("warnings %q, want one naming cap_dac_read_search", w)
	}
}

// asError requires err to be an *Error with code c.
func asError(t *testing.T, err error, c Code) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", c)
	}
	e, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("error %T %q is not an *Error", err, err)
	}
	if e.Code != c {
		t.Fatalf("code %s (%q), want %s", e.Code, e, c)
	}
	return e
}
