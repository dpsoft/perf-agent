package gpuprobe

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/cilium/ebpf"
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
	defer m.Close()

	rd, err := ringbuf.NewReader(m)
	require.NoError(t, err, "open ringbuf reader")
	defer rd.Close()

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
