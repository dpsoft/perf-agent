package perfmcp

import (
	"encoding/json"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// byState is the part of the page state that says who made what.
type byState struct {
	Current      string `json:"current"`
	By           string `json:"by"`
	Publications []struct {
		ID string `json:"id"`
		By string `json:"by"`
	} `json:"publications"`
}

// stateBy decodes the snapshot's state, which must carry by on the
// steering and on every publication, even empty.
func stateBy(t *testing.T, s *Server) byState {
	t.Helper()
	raw := s.live.snapshot().state
	var keys struct {
		By           *string `json:"by"`
		Publications []struct {
			By *string `json:"by"`
		} `json:"publications"`
	}
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if keys.By == nil {
		t.Errorf("the state has no by: %s", raw)
	}
	for i, p := range keys.Publications {
		if p.By == nil {
			t.Errorf("publication %d has no by: %s", i, raw)
		}
	}
	var st byState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// Each publication records the session that made it, and the steering
// the session that last moved it: two agents sharing one page can tell
// whose page is on screen.
func TestPublicationsAndSteeringRecordBy(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic(), "cpu-new.pb.gz": pmfixture.CPUBasicNew()})
	if st := stateBy(t, s); st.By != "" || len(st.Publications) != 0 {
		t.Errorf("the zero state: %+v", st)
	}
	claude := newMCPSession(t, s, "claude-code")
	codex := newMCPSession(t, s, "codex")
	var base, next openedAs
	claude.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &base)
	codex.callOK("open", map[string]any{"path": "cpu-new.pb.gz"}, &next)

	claude.callOK("page_explain", map[string]any{"handle": base.Handle, "view": base.DefaultView, "explanations": []any{}}, nil)
	if st := stateBy(t, s); st.Current != "p1" || st.By != "claude-code" || len(st.Publications) != 1 || st.Publications[0].By != "claude-code" {
		t.Errorf("after claude-code's page_explain: %+v", st)
	}
	codex.callOK("page_diff", map[string]any{"base_handle": base.Handle, "base_view": base.DefaultView, "new_handle": next.Handle, "new_view": next.DefaultView}, nil)
	if st := stateBy(t, s); st.Current != "p2" || st.By != "codex" || len(st.Publications) != 2 || st.Publications[1].By != "codex" {
		t.Errorf("after codex's page_diff: %+v", st)
	}
	codex.callOK("page_show", map[string]any{"publication": "p1"}, nil)
	st := stateBy(t, s)
	if st.Current != "p1" || st.By != "codex" {
		t.Errorf("after codex's page_show: %+v", st)
	}
	for _, p := range st.Publications {
		if want := map[string]string{"p1": "claude-code", "p2": "codex"}[p.ID]; p.By != want {
			t.Errorf("publication %s by %q, want %q", p.ID, p.By, want)
		}
	}
	// A publication is made by one session, whoever shows it later.
	if p := s.live.snapshot().pubs["p1"]; p.by != "claude-code" {
		t.Errorf("p1.by = %q", p.by)
	}
	// page_open's rows name who made each publication too.
	var opened struct {
		Publications []struct {
			ID string `json:"id"`
			By string `json:"by"`
		} `json:"publications"`
	}
	claude.callOK("page_open", map[string]any{}, &opened)
	if len(opened.Publications) != 2 || opened.Publications[0].By != "claude-code" || opened.Publications[1].By != "codex" {
		t.Errorf("page_open's rows: %+v", opened.Publications)
	}
}
