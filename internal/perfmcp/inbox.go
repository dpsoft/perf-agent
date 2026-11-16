package perfmcp

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// The kinds of a page report.
const (
	reportSelect = "select" // the user selected a frame, or none
	reportFocus  = "focus"  // the user laid the graph out from a frame, or the whole view
	reportAsk    = "ask"    // the user asked their agent about a frame
)

// askDuplicateWindow is how long an ask of the same publication and node
// as a kept one is taken for the same click.
const askDuplicateWindow = 2 * time.Second

// maxInboxName is the most bytes of a frame's name the inbox keeps.
const maxInboxName = 1 << 10

// askNonceBytes is the size of the per-process part of an ask ID.
const askNonceBytes = 4

// PageReport is what the live page tells the server the user did. Kind is
// select, focus or ask. Publication is the publication the page showed.
// Node is the frame the report is about: the frame selected for select
// ("" for none), the frame the layout now starts at for focus ("" for
// the whole view), and the frame asked about for ask. Focus is the frame
// the page's layout started at, "" for the whole view.
type PageReport struct {
	Kind, Publication, Node, Focus string
}

// ErrBadReport is Server.Report's error for a report that does not match
// the current snapshot; it is wrapped with the reason, which never repeats
// what the page sent.
var ErrBadReport = errors.New("invalid page report")

// Report records what the user did on the page, after validating it
// against the current snapshot: the publication must be kept, and each
// node a node of it, looked up as PageView.Frame looks it up. Every value
// the inbox keeps is the server's own: the IDs and the frame's name (cut
// to 1 KiB) from the publication, its view and digest, and the time. A
// select or focus replaces the user's last one; an ask joins the ask ring,
// unless one of the same publication and node was kept less than 2 s ago,
// when it is taken for the same click and Report returns nil. It never
// takes the engine, so it answers while a tool holds it. Any goroutine
// may call it.
func (s *Server) Report(r PageReport) error {
	if s.live == nil {
		return fmt.Errorf("%w: this server has no live page", ErrBadReport)
	}
	got, err := resolveReport(s.live.snapshot(), r)
	if err != nil {
		return err
	}
	s.live.inbox.record(got, s.clock())
	return nil
}

// reported is a validated report in the server's own values.
type reported struct {
	kind, publication, node, name, focus, view, digest string
}

// resolveReport validates r against snap and returns its values as the
// snapshot holds them.
func resolveReport(snap *liveSnapshot, r PageReport) (reported, error) {
	bad := func(why string) (reported, error) {
		return reported{}, fmt.Errorf("%w: %s", ErrBadReport, why)
	}
	switch r.Kind {
	case reportSelect, reportFocus, reportAsk:
	default:
		return bad("kind must be select, focus or ask")
	}
	p, err := PageView{snap: snap}.pub(r.Publication)
	if err != nil {
		return bad("the publication is not on the page; the page will refresh")
	}
	if r.Kind == reportAsk && r.Node == "" {
		return bad("an ask names a frame")
	}
	out := reported{kind: r.Kind, publication: p.id, view: p.view}
	if p.kind == pubExplained && p.prof != nil {
		out.digest = p.digest.String()
	}
	var ok bool
	if r.Node != "" {
		if out.node, out.name, ok = p.lookup(r.Node); !ok {
			return bad("the frame is not in the publication; the page will refresh")
		}
	}
	if r.Focus != "" {
		if out.focus, _, ok = p.lookup(r.Focus); !ok {
			return bad("the focused frame is not in the publication; the page will refresh")
		}
	}
	return out, nil
}

// lookup is node of p, by the lookups hasNode makes: its ID and its
// frame's name, cut to maxInboxName, both the publication's own strings.
func (p *publication) lookup(node string) (id, name string, ok bool) {
	if !p.hasNode(node) {
		return "", "", false
	}
	if p.kind == pubDiff {
		n, _ := p.index.Node(profilemodel.UnionID(node))
		return string(n.UnionID()), cutName(n.Name), true
	}
	n, _ := p.v.Node(profilemodel.NodeID(node))
	return string(n.ID), cutName(n.Function.Name), true
}

// cutName is name in at most maxInboxName bytes, cut at a character
// boundary and ending in "…" when it was cut.
func cutName(name string) string {
	if len(name) <= maxInboxName {
		return name
	}
	cut := maxInboxName - len("…")
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + "…"
}

// userView is the user's last select or focus.
type userView struct {
	publication, selected, name, focus string
	at                                 time.Time
}

// inboxAsk is one kept ask. readBy is the label of the session whose
// page_state first returned it, "" until one did.
type inboxAsk struct {
	n                                     int64
	publication, node, name, view, digest string
	at                                    time.Time
	readBy                                string
}

// inbox is what the user reported from the page: the last select or
// focus, and a ring of the newest asks. Everything is behind mu, which is
// never held while waiting for anything but the state bus; each change
// bumps rev and hands the inbox's state to the bus.
type inbox struct {
	mu  sync.Mutex
	bus *stateBus
	// max is how many asks the ring keeps; nonce the per-process part of
	// every ask ID, so an ID from another run is never taken for one of
	// this run's.
	max   int
	nonce string

	rev  int64
	user *userView
	// asks are the kept asks, oldest first. next is the number of the last
	// ask made and dropped counts the asks the ring let go.
	asks    []inboxAsk
	next    int64
	dropped int64
}

// newInbox is an empty inbox keeping size asks, whose IDs end in nonce.
func newInbox(size int, nonce string) *inbox {
	return &inbox{max: size, nonce: nonce}
}

// inboxState is the inbox part of the envelope: its revision, how many
// asks are kept and how many of them no session has read, and who read
// the newest one ("" for nobody yet).
type inboxState struct {
	Rev    string `json:"rev"`
	Asks   string `json:"asks"`
	Unread string `json:"unread"`
	ReadBy string `json:"read_by"`
}

// state is the inbox's state JSON. The caller holds in.mu, or in is not
// shared yet.
func (in *inbox) state() []byte {
	unread := 0
	for _, a := range in.asks {
		if a.readBy == "" {
			unread++
		}
	}
	st := inboxState{Rev: strconv.FormatInt(in.rev, 10), Asks: strconv.Itoa(len(in.asks)), Unread: strconv.Itoa(unread)}
	if len(in.asks) > 0 {
		st.ReadBy = in.asks[len(in.asks)-1].readBy
	}
	out, err := marshal(st)
	if err != nil {
		// Strings always encode.
		panic(err)
	}
	return out
}

// changed bumps rev and hands the new state to the bus. The caller holds
// in.mu.
func (in *inbox) changed() {
	in.rev++
	in.bus.setInbox(in.rev, in.state())
}

// record keeps r, made at now.
func (in *inbox) record(r reported, now time.Time) {
	in.mu.Lock()
	defer in.mu.Unlock()
	switch r.kind {
	case reportSelect:
		in.user = &userView{publication: r.publication, selected: r.node, name: r.name, focus: r.focus, at: now}
	case reportFocus:
		u := &userView{publication: r.publication, focus: r.node, at: now}
		if in.user != nil && in.user.publication == r.publication {
			u.selected, u.name = in.user.selected, in.user.name
		}
		in.user = u
	case reportAsk:
		if slices.ContainsFunc(in.asks, func(a inboxAsk) bool {
			d := now.Sub(a.at)
			return a.publication == r.publication && a.node == r.node && d < askDuplicateWindow && d > -askDuplicateWindow
		}) {
			return
		}
		if len(in.asks) >= in.max {
			in.asks = slices.Delete(in.asks, 0, len(in.asks)-in.max+1)
			in.dropped++
		}
		in.next++
		in.asks = append(in.asks, inboxAsk{n: in.next, publication: r.publication, node: r.node, name: r.name,
			view: r.view, digest: r.digest, at: now})
	}
	in.changed()
}

// askID is the ID of ask n of this run.
func (in *inbox) askID(n int64) string { return "i" + strconv.FormatInt(n, 10) + "-" + in.nonce }

// parseAskID reads an ask ID, i<N>-<8 hex digits>.
func parseAskID(id string) (n int64, nonce string, ok bool) {
	rest, ok := strings.CutPrefix(id, "i")
	if !ok {
		return 0, "", false
	}
	num, nonce, ok := strings.Cut(rest, "-")
	if !ok || num == "" || num[0] == '0' || len(nonce) != 2*askNonceBytes ||
		strings.ContainsFunc(num, func(c rune) bool { return c < '0' || c > '9' }) ||
		strings.ContainsFunc(nonce, func(c rune) bool { return (c < '0' || c > '9') && (c < 'a' || c > 'f') }) {
		return 0, "", false
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return n, nonce, true
}

// askRow is an ask as page_state lists it.
type askRow struct {
	ID          string `json:"id"`
	Publication string `json:"publication"`
	Node        string `json:"node"`
	Name        string `json:"name"`
	View        string `json:"view"`
	Digest      string `json:"digest"`
	At          string `json:"at"`
	ReadBy      string `json:"read_by"`
}

// userRow is the user's last select or focus as page_state shows it.
type userRow struct {
	Publication string `json:"publication"`
	Selected    string `json:"selected"`
	Name        string `json:"name"`
	Focus       string `json:"focus"`
	At          string `json:"at"`
}

// stamp is how page_state writes a time.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// inboxRead is what one page_state read takes from the inbox: up to limit
// asks, the user's last report (nil for none), the asks dropped, and
// whether since named another run's ask. ns are the asks' numbers, for
// markRead.
type inboxRead struct {
	rows    []askRow
	ns      []int64
	user    *userRow
	dropped int64
	foreign bool
}

// read takes up to limit kept asks: with newest, the newest ones;
// otherwise the oldest ones after since, an ask ID of this run, or from
// the oldest kept one when since is "" or another run's. Each row's
// read_by is label when no session read it yet, as markRead will record
// it. It marks nothing.
func (in *inbox) read(since string, limit int, newest bool, label string) (inboxRead, error) {
	var after int64
	var foreign bool
	if since != "" {
		n, nonce, ok := parseAskID(since)
		if !ok {
			return inboxRead{}, fmt.Errorf("since must be an ask ID as page_state returned it, such as i7-%s", strings.Repeat("0", 2*askNonceBytes))
		}
		if nonce == in.nonce {
			after = n
		} else {
			foreign = true
		}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	asks := in.asks
	if newest {
		asks = asks[max(0, len(asks)-limit):]
	} else {
		i, _ := slices.BinarySearchFunc(asks, after+1, func(a inboxAsk, n int64) int { return cmp.Compare(a.n, n) })
		asks = asks[i:min(len(asks), i+limit)]
	}
	out := inboxRead{rows: []askRow{}, dropped: in.dropped, foreign: foreign}
	for _, a := range asks {
		out.rows = append(out.rows, askRow{ID: in.askID(a.n), Publication: a.publication, Node: a.node, Name: a.name,
			View: a.view, Digest: a.digest, At: stamp(a.at), ReadBy: cmp.Or(a.readBy, label)})
		out.ns = append(out.ns, a.n)
	}
	if u := in.user; u != nil {
		out.user = &userRow{Publication: u.publication, Selected: u.selected, Name: u.name, Focus: u.focus, At: stamp(u.at)}
	}
	return out, nil
}

// markRead records label as the reader of the asks numbered ns that are
// still kept and that no session read yet; a change bumps rev once.
func (in *inbox) markRead(ns []int64, label string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	moved := false
	for i := range in.asks {
		if a := &in.asks[i]; a.readBy == "" && slices.Contains(ns, a.n) {
			a.readBy = label
			moved = true
		}
	}
	if moved {
		in.changed()
	}
}
