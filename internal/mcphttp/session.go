package mcphttp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// session is one MCP session: its protocol state and its calls in flight.
type session struct {
	id    string
	proto *mcpstdio.Session
	// used is the transport's tick at the session's last request, which
	// orders eviction; calls holds the cancel of each request in flight,
	// by its id as compact JSON; admitted counts those in the call class.
	// All are guarded by Transport.mu.
	used     uint64
	calls    map[string]context.CancelFunc
	admitted int
}

// newSessionID is 32 random bytes as unpadded base64url: 43 characters.
// A session id is not a credential: the bearer token gates the endpoint
// before any lookup, so a plain map lookup is fine.
func newSessionID() string {
	var b [32]byte
	// crypto/rand.Read never fails: it crashes the program instead.
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// lookup is the session r names. Without exactly one Mcp-Session-Id it
// answers 400, and for an id it does not hold 404, so a client whose
// session is gone (evicted, deleted or from before a restart)
// re-initializes. Called with t.mu held.
func (t *Transport) lookup(r *http.Request) (*session, int) {
	ids := r.Header.Values(HeaderSession)
	if len(ids) != 1 || ids[0] == "" {
		return nil, http.StatusBadRequest
	}
	s, ok := t.sessions[ids[0]]
	if !ok {
		return nil, http.StatusNotFound
	}
	t.touch(s)
	return s, 0
}

// touch marks s as the most recently used session. Called with t.mu held.
func (t *Transport) touch(s *session) {
	t.tick++
	s.used = t.tick
}

// admit adds s to the table, evicting the least recently used session
// with no call in flight when the table is full. It reports whether s was
// added, false when every session has a call in flight, and how many
// sessions it evicted, for the caller to log once t.mu is released.
// Called with t.mu held.
func (t *Transport) admit(s *session) (added bool, evicted int) {
	for len(t.sessions) >= t.o.MaxSessions {
		var victim *session
		for _, c := range t.sessions {
			if len(c.calls) == 0 && (victim == nil || c.used < victim.used) {
				victim = c
			}
		}
		if victim == nil {
			return false, evicted
		}
		delete(t.sessions, victim.id)
		evicted++
	}
	t.sessions[s.id] = s
	t.touch(s)
	return true, evicted
}

// remove ends s: its calls are cancelled and it leaves the table. Its
// calls still answer, under their cancelled contexts, and unregister
// themselves. Called with t.mu held.
func (t *Transport) remove(s *session) {
	for _, cancel := range s.calls {
		cancel()
	}
	delete(t.sessions, s.id)
}
