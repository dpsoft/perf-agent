package mcphttp

import (
	"bytes"
	"context"
	"encoding/json"
)

// compactID is a JSON-RPC id as compact JSON, the key a call is held under
// in its session, so 7 and "7" are distinct and spacing is not.
func compactID(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// register holds cancel under s and key, and reports false, holding
// nothing, when s already has a call in flight with that id. Called with
// t.mu held.
func (t *Transport) register(s *session, key string, cancel context.CancelFunc) bool {
	if _, dup := s.calls[key]; dup {
		return false
	}
	s.calls[key] = cancel
	t.inflight++
	return true
}

// unregister drops the call held under s and key. Called with t.mu held.
func (t *Transport) unregister(s *session, key string) {
	if _, ok := s.calls[key]; ok {
		delete(s.calls, key)
		t.inflight--
	}
}

// cancelNamed cancels the call notifications/cancelled names in s, and
// nothing in another session: ids are the client's, so another session's
// call may carry the same one. An unknown id is ignored, as is a call
// that already answered. Called with t.mu held.
func (t *Transport) cancelNamed(s *session, params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	if cancel, ok := s.calls[compactID(p.RequestID)]; ok {
		cancel()
	}
}
