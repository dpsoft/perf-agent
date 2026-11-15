package capture

import (
	"strings"
	"testing"
)

// status builds a /proc/self/status excerpt with the fields CheckSelf reads.
func status(uid, capEff, capPrm, noNewPrivs string) []byte {
	return []byte("Name:\tperf-agent-mcp\n" +
		"Uid:\t" + uid + "\n" +
		"Gid:\t1000\t1000\t1000\t1000\n" +
		"CapInh:\t0000000000000000\n" +
		"CapPrm:\t" + capPrm + "\n" +
		"CapEff:\t" + capEff + "\n" +
		"CapBnd:\t000001ffffffffff\n" +
		"NoNewPrivs:\t" + noNewPrivs + "\n" +
		"Seccomp:\t0\n")
}

const zero = "0000000000000000"

func TestCheckSelf(t *testing.T) {
	if err := CheckSelf(status("1000\t1000\t1000\t1000", zero, zero, "0")); err != nil {
		t.Fatalf("uid 1000, no capabilities, NoNewPrivs 0: %v", err)
	}
	for _, tc := range []struct {
		name   string
		status []byte
		want   string
	}{
		{"effective uid 0", status("1000\t0\t1000\t1000", zero, zero, "0"), "euid 0"},
		{"CapEff", status("1000\t1000\t1000\t1000", "0000000000000400", "0000000000000400", "0"), "CapEff 0000000000000400"},
		{"CapPrm", status("1000\t1000\t1000\t1000", zero, "0000000000200000", "0"), "CapPrm 0000000000200000"},
		{"NoNewPrivs", status("1000\t1000\t1000\t1000", zero, zero, "1"), "this server inherited no_new_privs (a sandbox?); perf-agent's file capabilities cannot apply"},
		{"no Uid", []byte("CapPrm:\t" + zero + "\nCapEff:\t" + zero + "\nNoNewPrivs:\t0\n"), "Uid"},
		{"bad CapEff", status("1000\t1000\t1000\t1000", "zz", zero, "0"), "CapEff"},
		{"non-numeric effective uid", status("1000 nope 1000 1000", zero, zero, "0"), "Uid"},
		{"negative effective uid", status("1000 -1 1000 1000", zero, zero, "0"), "Uid"},
		{"effective uid past uid_t", status("1000 4294967296 1000 1000", zero, zero, "0"), "Uid"},
		{"truncated Uid", status("1000 1000", zero, zero, "0"), "Uid"},
		{"five Uid fields", status("1000 1000 1000 1000 1000", zero, zero, "0"), "Uid"},
		{"effective uid 00", status("1000 00 1000 1000", zero, zero, "0"), "euid 0"},
		{"a line past the scanner's limit", append(status("1000 1000 1000 1000", zero, zero, "0"), strings.Repeat("x", 65537)...), "/proc/self/status"},
		{"Uid twice", append(status("1000\t1000\t1000\t1000", zero, zero, "0"), "Uid:\t1000\t0\t1000\t1000\n"...), "Uid"},
		{"no NoNewPrivs", []byte("Uid:\t1000\t1000\t1000\t1000\nCapPrm:\t" + zero + "\nCapEff:\t" + zero + "\n"), "NoNewPrivs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := asError(t, CheckSelf(tc.status), CodeDisabled)
			if !strings.Contains(e.Message, tc.want) {
				t.Fatalf("message %q does not say %q", e.Message, tc.want)
			}
		})
	}
}

// A file whose permitted set the bounding set would cut fails execve with
// EPERM, so the server refuses to capture with that perf-agent.
func TestCheckBounding(t *testing.T) {
	need := capSet(CapPerfmon, CapBPF, CapSysAdmin)
	withBnd := func(bnd string) []byte {
		return []byte("Uid:\t1000\t1000\t1000\t1000\nCapBnd:\t" + bnd + "\nNoNewPrivs:\t0\n")
	}
	if err := CheckBounding(withBnd("000001ffffffffff"), need); err != nil {
		t.Fatalf("full bounding set: %v", err)
	}
	if err := CheckBounding(withBnd(zero), 0); err != nil {
		t.Fatalf("no capability needed: %v", err)
	}
	for _, tc := range []struct {
		name   string
		status []byte
		want   string
	}{
		{"no cap_bpf", withBnd("0000007fffffffff"), "cap_bpf"},
		{"empty", withBnd(zero), "cap_sys_admin,cap_perfmon,cap_bpf"},
		{"no CapBnd", []byte("Uid:\t1000\t1000\t1000\t1000\n"), "CapBnd"},
		{"bad CapBnd", withBnd("zz"), "CapBnd"},
		{"CapBnd twice", append(withBnd("000001ffffffffff"), "CapBnd:\t0\n"...), "CapBnd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := asError(t, CheckBounding(tc.status, need), CodeDisabled)
			if !strings.Contains(e.Message, tc.want) {
				t.Fatalf("message %q does not say %q", e.Message, tc.want)
			}
		})
	}
}
