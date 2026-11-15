package perfmcp

import "testing"

// Two results of MaxResponseBytes fit the output queue, so a result answered
// by the dispatcher never leaves a reader-side answer (Control, busy) to be
// refused as a dead client. Raising MaxResponseBytes without the output
// bound fails here.
func TestAResultFitsHalfTheOutputQueue(t *testing.T) {
	for _, n := range []int{DefaultLimits().MaxResponseBytes, 1024, 8 << 20} {
		cfg := testConfig(t)
		cfg.Limits.MaxResponseBytes = n
		s := must(New(cfg))(t)
		if got := s.mcp.MaxResultBytes(); got < n {
			t.Errorf("MaxResponseBytes %d: the output queue holds two results of at most %d bytes", n, got)
		}
		_ = s.Close()
	}
}
