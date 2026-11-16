package perfmcp

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// PageConfig turns the live page engine on: the page_* tools, the
// publications and the snapshot a PageView reads. The server never
// listens; whoever serves the page sets it.
type PageConfig struct {
	// URL is the page's address, what page_open and every page tool
	// return.
	URL string
	// Notify receives every new state envelope, one line of JSON,
	// {"engine":<state>,"inbox":<inbox state>}, in the order they were
	// built: one for each state the engine commits and one for each
	// change of the inbox. Each part has its own revision, engine.seq and
	// inbox.rev, and neither ever goes back. It runs under the state bus's
	// lock, and under the engine or the inbox's lock, and must never block;
	// the envelope is shared and must not be modified. Nil drops them.
	Notify func(state []byte)
	// Viewers counts the page streams open, for the page tools' viewers.
	// Nil counts 0.
	Viewers func() int
}

// ErrPageNotFound is a PageView's answer for a publication or node its
// snapshot does not hold, an unknown node of a view, and an ID longer
// than any it holds: the page fetched it from a state that is no longer
// current, and refreshes from the next one.
var ErrPageNotFound = errors.New("no such publication or node; the page will refresh")

// PageLimitError is a model, frame or export that cannot be built within
// the page limits. Msg names the limit; a page answers it with 422.
type PageLimitError struct{ Msg string }

func (e *PageLimitError) Error() string { return e.Msg }

// maxPageID is the longest publication or node ID a PageView looks up; a
// longer one is not found without a lookup.
const maxPageID = 64

// PageView is one committed snapshot of the live page: its state, its
// publications and their histories, none of which changes once
// committed. Any goroutine may use it, and none of its methods changes
// server state. The bytes it returns are shared with the snapshot and
// must not be modified. The zero PageView, a server's without Page,
// holds nothing: State is nil and every other method answers
// ErrPageNotFound.
type PageView struct {
	snap *liveSnapshot
	// env is the state envelope when the view was taken.
	env []byte
	// cmd is the binary the server runs as, which a limit's advice names;
	// "" means perf-agent-mcp.
	cmd string
}

// PageSnapshot is the snapshot the engine holder last committed. Any
// goroutine may call it.
func (s *Server) PageSnapshot() PageView {
	if s.live == nil {
		return PageView{}
	}
	return PageView{snap: s.live.snapshot(), env: s.live.bus.envelope(), cmd: s.command()}
}

// ClaimPage marks s's live page as served. A page has one set of routes:
// the bounds they keep, such as how many exports run at once, hold for the
// server only while nothing else serves it. It fails on a server without
// Page, and on every call after the first. Any goroutine may call it.
func (s *Server) ClaimPage() error {
	if s.live == nil {
		return errors.New("perfmcp: this server has no live page: Config.Page is nil")
	}
	if !s.live.claimed.CompareAndSwap(false, true) {
		return errors.New("perfmcp: this server's live page is already served")
	}
	return nil
}

// State is the page's state envelope when the view was taken,
// {"engine":<state>,"inbox":<inbox state>}: engine is the committed state,
// what the page shows and the publications it can switch to, at its seq;
// inbox counts the user's asks, at its own rev. engine may be a commit
// older than the view's other methods read: the page fetches from the
// state it holds, and a publication that is gone since answers
// ErrPageNotFound.
func (v PageView) State() []byte {
	if v.snap == nil {
		return nil
	}
	return v.env
}

// Model is publication pub's page model.
func (v PageView) Model(pub string) ([]byte, error) {
	p, err := v.pub(pub)
	if err != nil {
		return nil, err
	}
	return p.model, nil
}

// History is publication pub's history of rejected attempts; a
// publication without one, a diff, has an empty history at version 0.
func (v PageView) History(pub string) ([]byte, error) {
	p, err := v.pub(pub)
	if err != nil {
		return nil, err
	}
	if h := v.snap.history[p.id]; h != nil {
		return h.json, nil
	}
	return emptyHistory(p.id), nil
}

// Frame is node of publication pub with its path from the root: a node ID
// of an explained publication's view, or a union ID of a diff's. A frame
// over MaxPageBytes is a *PageLimitError.
func (v PageView) Frame(pub, node string) ([]byte, error) {
	p, err := v.node(pub, node)
	if err != nil {
		return nil, err
	}
	var out []byte
	limit := p.opts.MaxBytes
	if p.kind == pubDiff {
		limit = p.diffOpts.MaxBytes
		out, err = flamegraph.DiffLiveFrame(p.diff, p.index, profilemodel.UnionID(node))
	} else {
		out, err = flamegraph.ExplainedLiveFrame(p.v, p.result, profilemodel.NodeID(node), p.opts.Digest)
	}
	if err != nil {
		return nil, computed(err)
	}
	// A frame's path names every caller, so a deep stack of long names
	// makes it as large as the stack: it is bounded like a model.
	if limit > 0 && len(out) > limit {
		return nil, &PageLimitError{fmt.Sprintf("the frame and its path from the root take %d bytes, over this server's MaxPageBytes=%d, so the page cannot show it; "+
			"restart %s with a larger --limit MaxPageBytes=N to see it", len(out), limit, cmp.Or(v.cmd, defaultCommand))}
	}
	return out, nil
}

// Focus is publication pub's page model with its layout starting at node.
// A model that cannot be built is the engine's error, which the page
// shows as one it cannot build.
func (v PageView) Focus(pub, node string) ([]byte, error) {
	p, err := v.node(pub, node)
	if err != nil {
		return nil, err
	}
	var out []byte
	if p.kind == pubDiff {
		opts := p.diffOpts
		opts.Focus = profilemodel.UnionID(node)
		out, err = flamegraph.DiffLiveModel(p.diff, opts)
	} else {
		opts := p.opts
		opts.Focus = profilemodel.NodeID(node)
		out, err = flamegraph.ExplainedLiveModel(p.v, p.result, opts)
	}
	if err != nil {
		return nil, computed(err)
	}
	return out, nil
}

// Export is publication pub as a static page: an explained one with the
// rejected attempts its history keeps, a diff as WriteDiffPage writes it.
// It is rendered whole before it is returned. One over the page limits
// is a *PageLimitError; any other failure says nothing of the limits.
func (v PageView) Export(pub string) ([]byte, error) {
	p, err := v.pub(pub)
	if err != nil {
		return nil, err
	}
	if p.kind == pubDiff {
		return exportDiff(p)
	}
	return exportExplained(p, v.snap.history[p.id])
}

// pub is the snapshot's publication id.
func (v PageView) pub(id string) (*publication, error) {
	if v.snap == nil || len(id) > maxPageID {
		return nil, ErrPageNotFound
	}
	p := v.snap.pubs[id]
	if p == nil {
		return nil, ErrPageNotFound
	}
	return p, nil
}

// node is the snapshot's publication id, when it may hold node: any node
// ID of an explained publication, whose view the engine checks, or a
// union ID a diff's index holds.
func (v PageView) node(id, node string) (*publication, error) {
	if len(node) > maxPageID {
		return nil, ErrPageNotFound
	}
	p, err := v.pub(id)
	if err != nil {
		return nil, err
	}
	if p.kind == pubDiff && !p.hasNode(node) {
		return nil, ErrPageNotFound
	}
	return p, nil
}

// computed is the error a frame or model the engine could not build
// answers with: not found for a node the view does not have, the
// engine's own error otherwise.
func computed(err error) error {
	if errors.Is(err, profilemodel.ErrUnknownNode) {
		return ErrPageNotFound
	}
	return err
}

// exportExplained is p as the static explained page, with h, the rejected
// attempts its history keeps (nil for none), within the page limits. It is
// rendered whole, once, before anything is sent, so a failure sends no
// page. One over the limits is a *PageLimitError; any other failure says
// nothing of the limits. The rows h left out are counted on their
// attempts and in its dropped attempts, so the page says how many it does
// not list. Like the live page, it words the history for attempts made
// before the publication or after it.
func exportExplained(p *publication, h *liveHistory) ([]byte, error) {
	opts := flamegraph.ExplainedPageOptions{
		Title: p.opts.Title, ProfileName: p.opts.ProfileName, Digest: p.opts.Digest, Attempt: p.opts.Attempt,
		MaxNodes: p.opts.MaxNodes, MaxBytes: p.opts.MaxBytes, LiveHistory: true,
	}
	if h != nil {
		opts.Rejected, opts.DroppedRejected, opts.HistoryIncomplete = h.rejected, h.dropped, h.incomplete
	}
	page, err := flamegraph.ExplainedPage(p.v, p.result, opts)
	switch {
	case errors.Is(err, flamegraph.ErrPageTooLarge):
		return nil, &PageLimitError{fmt.Sprintf("the page cannot be exported within MaxPageBytes=%d: %v", p.opts.MaxBytes, err)}
	case err != nil:
		return nil, fmt.Errorf("the page cannot be exported: %v", err)
	case len(page) > p.opts.MaxBytes:
		return nil, &PageLimitError{fmt.Sprintf("the page is %d bytes, over MaxPageBytes=%d", len(page), p.opts.MaxBytes)}
	}
	return page, nil
}

// exportDiff is diff publication p as WriteDiffPage writes it, without
// the agent's title, whose numbers that page would show unmarked. It is
// bounded before any of it is built, against the limits the model's
// export state was judged by, and rendered whole before anything is sent.
// One over the limits is a *PageLimitError.
func exportDiff(p *publication) ([]byte, error) {
	var page bytes.Buffer
	err := flamegraph.WriteDiffPageBounded(&page, p.diff, p.export, p.diffOpts.MaxBytes, p.diffOpts.MaxNodes)
	switch {
	case errors.Is(err, flamegraph.ErrPageTooLarge):
		return nil, &PageLimitError{fmt.Sprintf("the diff cannot be exported within this server's MaxPageBytes=%d and MaxPageNodes=%d: %v",
			p.diffOpts.MaxBytes, p.diffOpts.MaxNodes, err)}
	case err != nil:
		return nil, fmt.Errorf("the diff cannot be exported: %v", err)
	}
	return page.Bytes(), nil
}

// emptyHistory is the history of a publication without one, a diff.
func emptyHistory(id string) []byte {
	out, err := json.Marshal(struct {
		Publication string   `json:"publication"`
		Version     string   `json:"version"`
		Rows        []string `json:"rows"`
	}{id, "0", []string{}})
	if err != nil {
		// A string and an empty list always encode.
		panic(err)
	}
	return out
}
