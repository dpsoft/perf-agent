package capture

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Capability is a Linux capability number.
type Capability uint

// The capabilities the binary policy names.
const (
	CapDACOverride       Capability = 1
	CapDACReadSearch     Capability = 2
	CapFowner            Capability = 3
	CapSetuid            Capability = 7
	CapSysModule         Capability = 16
	CapSysPtrace         Capability = 19
	CapSysAdmin          Capability = 21
	CapSyslog            Capability = 34
	CapPerfmon           Capability = 38
	CapBPF               Capability = 39
	CapCheckpointRestore Capability = 40
)

// capNames are the kernel's names for capabilities 0..40, as setcap and
// getcap print them.
var capNames = [...]string{
	"cap_chown", "cap_dac_override", "cap_dac_read_search", "cap_fowner",
	"cap_fsetid", "cap_kill", "cap_setgid", "cap_setuid", "cap_setpcap",
	"cap_linux_immutable", "cap_net_bind_service", "cap_net_broadcast",
	"cap_net_admin", "cap_net_raw", "cap_ipc_lock", "cap_ipc_owner",
	"cap_sys_module", "cap_sys_rawio", "cap_sys_chroot", "cap_sys_ptrace",
	"cap_sys_pacct", "cap_sys_admin", "cap_sys_boot", "cap_sys_nice",
	"cap_sys_resource", "cap_sys_time", "cap_sys_tty_config", "cap_mknod",
	"cap_lease", "cap_audit_write", "cap_audit_control", "cap_setfcap",
	"cap_mac_override", "cap_mac_admin", "cap_syslog", "cap_wake_alarm",
	"cap_block_suspend", "cap_audit_read", "cap_perfmon", "cap_bpf",
	"cap_checkpoint_restore",
}

// String returns the capability's name, or cap_N for a number this
// package does not know.
func (c Capability) String() string {
	if int(c) < len(capNames) {
		return capNames[c]
	}
	return "cap_" + strconv.FormatUint(uint64(c), 10)
}

// CapSet is a set of capabilities, bit N for capability N.
type CapSet uint64

// Has reports whether c is in the set.
func (s CapSet) Has(c Capability) bool {
	return c < 64 && s&(1<<c) != 0
}

// Names returns the names of the set's capabilities in bit order.
func (s CapSet) Names() []string {
	var names []string
	for c := range Capability(64) {
		if s.Has(c) {
			names = append(names, c.String())
		}
	}
	return names
}

func capSet(caps ...Capability) CapSet {
	var s CapSet
	for _, c := range caps {
		s |= 1 << c
	}
	return s
}

// The vfs_cap_data layout of the security.capability xattr
// (linux/capability.h): a little-endian magic word, a (permitted,
// inheritable) pair per 32 capabilities, and for revision 3 the rootid.
const (
	vfsCapRevisionMask   = 0xFF000000
	vfsCapRevision2      = 0x02000000
	vfsCapRevision3      = 0x03000000
	vfsCapFlagsEffective = 0x000001
	xattrCapsSize2       = 20
	xattrCapsSize3       = 24
)

// ParseFileCaps parses a security.capability value and returns its
// permitted set. It refuses what would not grant exactly that set to
// perf-agent when run by the user: a revision other than 2 or 3, a
// namespaced (rootid ≠ 0) capability, a missing effective flag, a
// non-empty inheritable set, or a length that does not match the revision.
func ParseFileCaps(b []byte) (CapSet, error) {
	if len(b) < 4 {
		return 0, fmt.Errorf("security.capability is %d bytes, too short for a magic word", len(b))
	}
	magic := binary.LittleEndian.Uint32(b)
	rev, flags := magic&vfsCapRevisionMask, magic&^vfsCapRevisionMask
	switch rev {
	case vfsCapRevision2:
		if len(b) != xattrCapsSize2 {
			return 0, fmt.Errorf("security.capability revision 2 is %d bytes, got %d", xattrCapsSize2, len(b))
		}
	case vfsCapRevision3:
		if len(b) != xattrCapsSize3 {
			return 0, fmt.Errorf("security.capability revision 3 is %d bytes, got %d", xattrCapsSize3, len(b))
		}
		if root := binary.LittleEndian.Uint32(b[20:]); root != 0 {
			return 0, fmt.Errorf("security.capability has rootid %d: capabilities set in a user namespace do not apply outside it", root)
		}
	default:
		return 0, fmt.Errorf("security.capability revision %#08x; want revision 2 or 3", rev)
	}
	if flags&^vfsCapFlagsEffective != 0 {
		return 0, fmt.Errorf("security.capability has unknown flags %#x", flags)
	}
	if flags&vfsCapFlagsEffective == 0 {
		return 0, fmt.Errorf("security.capability does not set the effective flag (+ep)")
	}
	word := func(off int) uint64 { return uint64(binary.LittleEndian.Uint32(b[off:])) }
	permitted := word(4) | word(12)<<32
	if inheritable := word(8) | word(16)<<32; inheritable != 0 {
		return 0, fmt.Errorf("security.capability has a non-empty inheritable set (%s)", strings.Join(CapSet(inheritable).Names(), ","))
	}
	return CapSet(permitted), nil
}

// SetcapLine is the command that gives perf-agent its capabilities, as
// docs/requirements.md documents it.
const SetcapLine = "setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep <path>"

var (
	// requiredCaps is the minimal set for kernels ≥ 5.9.
	requiredCaps = capSet(CapBPF, CapPerfmon, CapSysPtrace, CapCheckpointRestore)
	// allowedCaps may be present as well: cap_syslog enables kernel
	// stacks, cap_sys_admin is accepted, and cap_dac_read_search draws a
	// warning.
	allowedCaps = requiredCaps | capSet(CapSyslog, CapSysAdmin, CapDACReadSearch)
)

// checkCaps applies the binary policy to a permitted set. Any capability
// outside the allowed ones is refused by name: with DAC override, for
// one, a symlink planted in the staging path could make the child write
// anywhere.
func checkCaps(c CapSet) (warnings []string, err error) {
	if extra := c &^ allowedCaps; extra != 0 {
		return nil, Errorf(CodeDisabled, "give it exactly the needed set: "+SetcapLine,
			"perf-agent has capabilities capture does not allow: %s", strings.Join(extra.Names(), ", "))
	}
	if missing := requiredCaps &^ c; missing != 0 {
		return nil, Errorf(CodeDisabled, "run: "+SetcapLine,
			"perf-agent lacks the file capabilities %s", strings.Join(missing.Names(), ", "))
	}
	if c.Has(CapDACReadSearch) {
		warnings = append(warnings, "perf-agent has cap_dac_read_search, which only helps with other users' processes; capture refuses those")
	}
	return warnings, nil
}
