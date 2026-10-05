package gpuprobe_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A zero return address must END the walk and SAY SO, not become a frame.
//
// Source inspection rather than a driven walk, for the reason the neighbouring
// walker tests give: this arm is only reachable inside the BPF verifier, and
// reproducing it needs a CFA that points at a zeroed stack slot -- which is
// exactly the condition issue #185 produced by accident and nothing can
// produce on purpose from userspace.
//
// What #185 looked like without this guard: ret_addr was read as zero,
// assigned to ctx->pc, and pushed, so every arm64 stack came back
//
//	frame 0/1  the probe site
//	frame 2    0x0
//
// filed as `abandoned` with no counter naming a cause. The flag is the
// difference between that and a walk that says why it stopped.
func TestAZeroReturnAddressEndsTheWalkAndIsFlagged(t *testing.T) {
	src, err := os.ReadFile("../bpf/unwind_common.h")
	require.NoError(t, err)
	body := string(src)

	require.Contains(t, body, "#define WALKER_FLAG_RA_ZERO          0x100",
		"WALKER_FLAG_RA_ZERO is gone or renumbered; gpuprobe's walkerFlagRAZero and the __u16 walker_flags depend on this value")

	step := body[strings.Index(body, "static __always_inline long unwind_frame("):]
	require.NotEmpty(t, step, "unwind_frame not found in the shared header")

	// The guard must sit AFTER the read it is checking -- a zero cannot be
	// detected before the slot is read -- and the read must have succeeded,
	// so the guard belongs under the OFFSET_CFA arm rather than beside the
	// read-failure bail-out.
	read := strings.Index(step, "bpf_probe_read_user(&ret_addr")
	require.Positive(t, read, "the return-address read is gone or reshaped")

	guard := strings.Index(step, "if (ret_addr == 0) {")
	require.Positive(t, guard, "nothing checks the return address for zero; a zero would be pushed as a frame (#187)")
	require.Greater(t, guard, read, "the zero check runs before the read that produces the value")

	arm := step[guard:]
	stop := strings.Index(arm, "goto stop;")
	require.Positive(t, stop, "the zero arm does not stop the walk")

	flag := strings.Index(arm, "WALKER_FLAG_RA_ZERO")
	require.Positive(t, flag, "the zero arm stops without raising a flag, which is the unflagged `abandoned` bucket that hid #185")
	require.Less(t, flag, stop,
		"the flag must be raised BEFORE the stop, or a walk ending here is filed as one that merely ran out")

	// And the zero must never reach ctx->pc: that assignment is what turned it
	// into a frame.
	require.NotContains(t, arm[:stop], "ctx->pc = ret_addr;",
		"the zero is still assigned to ctx->pc, so it is still pushed as a frame")
}

// The widening that made the ninth bit possible. walker_flags held eight
// assigned bits; RA_ZERO needed a ninth, and the byte it took was already
// padding, so no other field moved.
func TestWalkerFlagsIsWideEnoughForEveryFlag(t *testing.T) {
	src, err := os.ReadFile("../bpf/unwind_record.h")
	require.NoError(t, err)
	body := string(src)

	require.Contains(t, body, "__u16 walker_flags;",
		"walker_flags is not __u16; WALKER_FLAG_RA_ZERO (0x100) does not fit in a __u8 and would be silently truncated")
	require.NotContains(t, body, "__u8  walker_flags;",
		"a __u8 walker_flags declaration survives; two declarations would disagree")

	// The pad byte it consumed must be gone, not duplicated -- otherwise the
	// struct grew and every offset after it moved.
	require.NotContains(t, body, "__u16 walker_flags; // bitmask of WALKER_FLAG_* (defined near walk_step)\n    __u8  _pad;\n",
		"the pad byte walker_flags absorbed is still declared, so the header grew")
}
