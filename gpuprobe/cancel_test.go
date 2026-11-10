package gpuprobe

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// hasBPFCaps mirrors gate_test.go's hasBPFAndPerfmon; it is duplicated rather
// than shared because gate_test.go is in the external gpuprobe_test package
// and this test needs the unexported Consumer.reader field. Permitted is
// checked as well as Effective: a setcap'd binary has not promoted Permitted
// yet, and gating on Getuid alone would skip on exactly the machine that can
// run this.
func hasBPFCaps() bool {
	if os.Geteuid() == 0 {
		return true
	}
	set := cap.GetProc()
	if set == nil {
		return false
	}
	for _, flag := range []cap.Flag{cap.Permitted, cap.Effective} {
		if have, err := set.GetFlag(flag, cap.BPF); err == nil && have {
			return true
		}
	}
	return false
}

// TestCancelInterruptsARealBlockedRingbufRead is the regression test for the
// cancellation deadlock. It uses a real *ringbuf.Reader over a real
// BPF_MAP_TYPE_RINGBUF map — no attach, no program, no probe — because the
// bug lives entirely in that type's locking: ReadInto holds the reader's
// mutex across its epoll wait, so SetDeadline, which wants the same mutex,
// parks behind the very read it is meant to interrupt. A fake reader that
// returned from Read immediately would have passed before the fix too, so the
// read here is genuinely blocked: nothing is ever written to the ring, and the
// test waits for the goroutine to be parked in the poller before cancelling.
//
// Creating the map needs CAP_BPF (this machine has
// kernel.unprivileged_bpf_disabled=2, and map creation returns EPERM without
// it), so the test skips unless the binary carries it — it runs under the same
// setcap'd binary as the phase gate. consumer_test.go's
// TestRunStopsOnContextCancel covers the same property unprivileged, against a
// fake that models the real reader's locking.
func TestCancelInterruptsARealBlockedRingbufRead(t *testing.T) {
	if !hasBPFCaps() {
		t.Skip("needs CAP_BPF to create a BPF_MAP_TYPE_RINGBUF map; setcap the test binary")
	}

	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       "cancel_test",
		Type:       ebpf.RingBuf,
		MaxEntries: 4096, // must be a power of two and page-multiple
	})
	require.NoError(t, err, "create ringbuf map")
	defer func() { _ = m.Close() }()

	rd, err := ringbuf.NewReader(m)
	require.NoError(t, err, "open ringbuf reader")
	defer func() { _ = rd.Close() }()

	c := newTestConsumer(&recordingSink{})
	c.reader = rd

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Nothing will ever be written to this ring, so Run parks in the poller's
	// epoll wait holding the reader's mutex. Give it time to get there:
	// cancelling before the read begins is the one case the broken
	// implementation also survived, and would make this test vacuous.
	time.Sleep(300 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "a closed reader is a clean exit for Run")
	case <-time.After(10 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("Run did not return after cancel: the blocked read was never interrupted.\n%s", buf[:n])
	}

	// The reader is already closed by cancellation. Consumer.Close must stay
	// safe and must not surface the second close as an error: ringbuf's Close
	// maps an already-closed poller to nil.
	assert.NoError(t, c.Close())
	assert.NoError(t, c.Close(), "Close is idempotent")
}

// ringbufWriter loads a socket filter whose only job is bpf_ringbuf_output of
// a zeroed 32-byte record into m. Test-running it n times fills the ring as a
// probe would, with no uprobe and no target process.
func ringbufWriter(t *testing.T, m *ebpf.Map) *ebpf.Program {
	t.Helper()
	const size = 32
	insns := asm.Instructions{}
	for off := int16(-size); off < 0; off += 8 {
		insns = append(insns, asm.StoreImm(asm.R10, off, 0, asm.DWord))
	}
	insns = append(insns,
		asm.LoadMapPtr(asm.R1, m.FD()),
		asm.Mov.Reg(asm.R2, asm.R10),
		asm.Add.Imm(asm.R2, -size),
		asm.Mov.Imm(asm.R3, size),
		asm.Mov.Imm(asm.R4, 0),
		asm.FnRingbufOutput.Call(),
		asm.Mov.Imm(asm.R0, 0),
		asm.Return(),
	)
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Type:         ebpf.SocketFilter,
		Instructions: insns,
		License:      "GPL",
	})
	require.NoError(t, err, "load ringbuf writer")
	t.Cleanup(func() { _ = prog.Close() })
	return prog
}

func fillRing(t *testing.T, prog *ebpf.Program, n int) {
	t.Helper()
	_, err := prog.Run(&ebpf.RunOptions{Data: make([]byte, 14), Repeat: uint32(n)})
	require.NoError(t, err, "test-run ringbuf writer")
}

// The stop against the real reader: records already in the ring when Run's
// context ends are delivered (Flush, then ErrFlushed once empty), not
// discarded by a Close. Each zeroed record fails to decode and is counted
// Malformed, which is enough to count what was read.
func TestStopDrainsARealBackloggedRing(t *testing.T) {
	if !hasBPFCaps() {
		t.Skip("needs CAP_BPF to create a ringbuf and load a program; setcap the test binary")
	}
	m, err := ebpf.NewMap(&ebpf.MapSpec{Name: "stop_test", Type: ebpf.RingBuf, MaxEntries: 1 << 20})
	require.NoError(t, err)
	defer func() { _ = m.Close() }()
	const n = 10_000
	fillRing(t, ringbufWriter(t, m), n)

	rd, err := ringbuf.NewReader(m)
	require.NoError(t, err)
	require.Equal(t, n*ringRecordBytes(32), rd.AvailableBytes(), "ring bytes per record")
	c := newTestConsumer(&recordingSink{})
	c.reader = rd

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish the drain")
	}

	st := c.Stats()
	assert.Equal(t, uint64(n), st.Batches+st.Malformed, "every record in the ring was read")
	assert.Zero(t, st.StopUnreadBytes)
	assert.NoError(t, c.Close())
}

// A drain cut short by its timeout counts exactly what it left in the ring,
// in the reader's own unit.
func TestStopTimeoutCountsTheRealRingRemainder(t *testing.T) {
	if !hasBPFCaps() {
		t.Skip("needs CAP_BPF to create a ringbuf and load a program; setcap the test binary")
	}
	m, err := ebpf.NewMap(&ebpf.MapSpec{Name: "stop_test", Type: ebpf.RingBuf, MaxEntries: 1 << 20})
	require.NoError(t, err)
	defer func() { _ = m.Close() }()
	const n = 10_000
	fillRing(t, ringbufWriter(t, m), n)

	rd, err := ringbuf.NewReader(m)
	require.NoError(t, err)
	c := newConsumer(Config{Sink: &recordingSink{}, StopDrainTimeout: time.Nanosecond})
	c.reader = rd

	// The deadline is set before Run reads anything, so it is already past
	// when the first record arrives.
	c.stopDeadline.Store(time.Now().UnixNano())
	require.NoError(t, c.Run(context.Background()))

	st := c.Stats()
	read := st.Batches + st.Malformed
	assert.Equal(t, uint64(n*ringRecordBytes(32)), read*uint64(ringRecordBytes(32))+st.StopUnreadBytes)
	assert.Less(t, read, uint64(n))
	assert.NoError(t, c.Close())
}
