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
	"unicode"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// The kinds of a page report.
const (
	reportSelect = "select" // the user selected a frame, or none
	reportFocus  = "focus"  // the user laid the graph out from a frame, or the whole view
	reportAsk    = "ask"    // the user asked their agent about a frame
)

// askDuplicateWindow is how long an ask of the same publication, node and
// question as a kept one is taken for the same click.
const askDuplicateWindow = 2 * time.Second

// maxQuestionRunes is the most characters an ask's question holds, after
// trimming.
const maxQuestionRunes = 280

// questionSource is how page_state labels an ask's question: the one value
// in the inbox that a person typed rather than the server made.
const questionSource = "typed on the page by the person viewing it"

// questionBreaks folds the line breaks and tabs of a question to spaces,
// so the agent always reads it as one line.
var questionBreaks = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ", "\u2028", " ", "\u2029", " ")

// questionMarks strips the directional marks (the left-to-right,
// right-to-left and Arabic letter marks) from a question: right-to-left
// text pasted from elsewhere carries them, and they change only how
// neighbouring punctuation is placed.
var questionMarks = strings.NewReplacer("\u200e", "", "\u200f", "", "\u061c", "")

// maxInboxName is the most bytes of a frame's name the inbox keeps.
const maxInboxName = 1 << 10

// askNonceBytes is the size of the per-process part of an ask ID.
const askNonceBytes = 4

// PageReport is what the live page tells the server the user did. Kind is
// select, focus or ask. Publication is the publication the page showed.
// Node is the frame the report is about: the frame selected for select
// ("" for none), the frame the layout now starts at for focus ("" for
// the whole view), and the frame asked about for ask. Focus is, for
// select, the frame the page's layout started at, "" for the whole view;
// the other kinds do not use it. Question is, for ask, the question the
// person typed about the frame, "" for none; the other kinds carry none.
type PageReport struct {
	Kind, Publication, Node, Focus, Question string
}

// ErrBadReport is Server.Report's error for a report that does not match
// the current snapshot; it is wrapped with the reason, which never repeats
// what the page sent.
var ErrBadReport = errors.New("invalid page report")

// Report records what the user did on the page, after validating it
// against the current snapshot: the publication must be kept, and each
// node a node of it, looked up as PageView.Frame looks it up. Every value
// the inbox keeps is the server's own: the IDs and the frame's name (cut
// to 1 KiB) from the publication, its view and digest, and the time; the
// one exception is an ask's question, which the person typed, kept as
// normalizeQuestion makes it and labelled as typed wherever it is shown.
// A select or focus replaces the user's last one, which page_state reads;
// the envelope does not show it, so the page is sent nothing, and one that
// repeats the last changes nothing. An ask joins the ask ring and is sent
// to the page, unless one of the same publication, node and question was
// kept less than 2 s ago, when it is taken for the same click and Report
// returns nil. It never takes the engine, so it answers while a tool
// holds it. Any goroutine may call it.
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
	kind, publication, node, name, focus, view, digest, question string
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
	if r.Kind != reportAsk && r.Question != "" {
		return bad("only an ask carries a question")
	}
	question, err := normalizeQuestion(r.Question)
	if err != nil {
		return bad(err.Error())
	}
	p, err := PageView{snap: snap}.pub(r.Publication)
	if err != nil {
		return bad("the publication is not on the page; the page will refresh")
	}
	if r.Kind == reportAsk && r.Node == "" {
		return bad("an ask names a frame")
	}
	out := reported{kind: r.Kind, publication: p.id, view: p.view, question: question}
	if p.kind == pubExplained && p.prof != nil {
		out.digest = p.digest.String()
	}
	var ok bool
	if r.Node != "" {
		if out.node, out.name, ok = p.lookup(r.Node); !ok {
			return bad("the frame is not in the publication; the page will refresh")
		}
	}
	if r.Kind == reportSelect && r.Focus != "" {
		if out.focus, _, ok = p.lookup(r.Focus); !ok {
			return bad("the focused frame is not in the publication; the page will refresh")
		}
	}
	return out, nil
}

// normalizeQuestion is q as the inbox keeps it: only text the person can
// see. Its line breaks, tabs and spaces of every width are folded to one
// space, its directional marks stripped and the space around it trimmed;
// "" is no question. It refuses q when it is not UTF-8, holds any other
// control character (C0, DEL or C1), a bidirectional embedding, override
// or isolate, an invisible character (see invisibleIn), or is left with
// more than 280 characters. The reason never repeats q.
func normalizeQuestion(q string) (string, error) {
	if !utf8.ValidString(q) {
		return "", errors.New("the question is not valid UTF-8")
	}
	q = questionMarks.Replace(questionBreaks.Replace(q))
	if strings.ContainsFunc(q, unicode.IsControl) {
		return "", errors.New("the question holds a control character; send it as plain text")
	}
	if strings.ContainsFunc(q, isBidiControl) {
		return "", errors.New("the question holds a bidirectional formatting character; send it as plain text")
	}
	// Every Zs space becomes U+0020, and Fields then collapses each run
	// of spaces to one and trims the ends.
	q = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Zs, r) {
			return ' '
		}
		return r
	}, q)), " ")
	if invisibleIn(q) {
		return "", errors.New("the question holds an invisible formatting character; send it as plain text")
	}
	if utf8.RuneCountInString(q) > maxQuestionRunes {
		return "", fmt.Errorf("the question is over %d characters", maxQuestionRunes)
	}
	return q, nil
}

// isBidiControl reports whether r is one of the characters that reorder
// the text around them: the embeddings, the overrides and the isolates.
func isBidiControl(r rune) bool {
	return r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069'
}

// isIgnorable reports whether r is a default-ignorable character that is
// not a format character: the combining grapheme joiner, the Hangul
// fillers, the Khmer inherent vowels, the Mongolian free variation
// selectors, the variation selectors, and the whole U+E0000 to U+E0FFF
// block, assigned or not.
func isIgnorable(r rune) bool {
	switch {
	case r == '\u034f', r == '\u115f', r == '\u1160', r == '\u17b4', r == '\u17b5', r == '\u3164', r == '\uffa0':
		return true
	case r >= '\u180b' && r <= '\u180f', r >= '\ufe00' && r <= '\ufe0f', r >= '\U000e0000' && r <= '\U000e0fff':
		return true
	}
	return false
}

// isJoiner reports whether r is the zero-width non-joiner or joiner.
func isJoiner(r rune) bool { return r == '\u200c' || r == '\u200d' }

// isVisible reports whether r is drawn on its own: neither a format
// character nor default-ignorable.
func isVisible(r rune) bool { return !unicode.Is(unicode.Cf, r) && !isIgnorable(r) }

// invisibleIn reports whether q holds a character that is not drawn but
// that an agent reading q would still read: any format character (Cf: the
// tag characters, which can spell a whole hidden sentence, zero-width
// spaces, the BOM, the soft hyphen, the interlinear annotations) or other
// default-ignorable character, with two exceptions that scripts and emoji
// need. One text or emoji presentation selector (U+FE0E, U+FE0F) may follow
// a visible character. A zero-width non-joiner or joiner may stand between
// two visible characters, never beside another joiner, so it carries no
// hidden run of bits; the character before a joiner may also be an emoji
// presentation selector that follows a visible one, as in an emoji ZWJ
// sequence such as the rainbow flag.
func invisibleIn(q string) bool {
	rs := []rune(q)
	visibleAt := func(i int) bool { return i >= 0 && i < len(rs) && isVisible(rs[i]) }
	for i, r := range rs {
		switch {
		case isJoiner(r):
			before := visibleAt(i-1) || i >= 1 && rs[i-1] == '\ufe0f' && visibleAt(i-2)
			if !before || !visibleAt(i+1) {
				return true
			}
		case r == '\ufe0e', r == '\ufe0f':
			if !visibleAt(i - 1) {
				return true
			}
		case !isVisible(r):
			return true
		}
	}
	return false
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
	n                                               int64
	publication, node, name, view, digest, question string
	at                                              time.Time
	readBy                                          string
}

// inbox is what the user reported from the page: the last select or
// focus, and a ring of the newest asks. Everything is behind mu, which is
// never held while waiting for anything but the state bus. A change the
// envelope shows (the asks kept, how many are unread, the newest one's
// reader) bumps rev and hands the inbox's state to the bus; the user's
// select or focus is read by page_state only, and moves neither.
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
// asks are kept and how many of them no session has read, who read the
// newest one ("" for nobody yet) and its question, absent for none.
type inboxState struct {
	Rev      string `json:"rev"`
	Asks     string `json:"asks"`
	Unread   string `json:"unread"`
	ReadBy   string `json:"read_by"`
	Question string `json:"question,omitzero"`
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
		newest := in.asks[len(in.asks)-1]
		st.ReadBy, st.Question = newest.readBy, newest.question
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
	case reportSelect, reportFocus:
		u := userView{publication: r.publication, selected: r.node, name: r.name, focus: r.focus, at: now}
		if r.kind == reportFocus {
			u.selected, u.name, u.focus = "", "", r.node
			if in.user != nil && in.user.publication == r.publication {
				u.selected, u.name = in.user.selected, in.user.name
			}
		}
		if in.user != nil {
			if was := *in.user; was.publication == u.publication && was.selected == u.selected && was.focus == u.focus {
				return
			}
		}
		in.user = &u
		return
	case reportAsk:
		if slices.ContainsFunc(in.asks, func(a inboxAsk) bool {
			d := now.Sub(a.at)
			return a.publication == r.publication && a.node == r.node && a.question == r.question &&
				d < askDuplicateWindow && d > -askDuplicateWindow
		}) {
			return
		}
		if len(in.asks) >= in.max {
			in.asks = slices.Delete(in.asks, 0, len(in.asks)-in.max+1)
			in.dropped++
		}
		in.next++
		in.asks = append(in.asks, inboxAsk{n: in.next, publication: r.publication, node: r.node, name: r.name,
			view: r.view, digest: r.digest, question: r.question, at: now})
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

// askRow is an ask as page_state lists it. Question and QuestionSource
// are absent for an ask without a question.
type askRow struct {
	ID             string `json:"id"`
	Publication    string `json:"publication"`
	Node           string `json:"node"`
	Name           string `json:"name"`
	View           string `json:"view"`
	Digest         string `json:"digest"`
	At             string `json:"at"`
	ReadBy         string `json:"read_by"`
	Question       string `json:"question,omitzero"`
	QuestionSource string `json:"question_source,omitzero"`
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
// asks, the user's last report (nil for none), the asks dropped, whether
// since was not one of this run's asks, and how many asks after since the
// ring let go before this read.
type inboxRead struct {
	rows    []askRow
	user    *userRow
	dropped int64
	foreign bool
	lost    int64
}

// read takes up to limit kept asks, and once answer has made its answer
// from them, records label as the reader of those no session read yet,
// all under one hold of the lock: each row's read_by is the reader the
// inbox keeps, so two sessions reading at once never both claim the first
// read. With newest it takes the newest asks; otherwise the oldest after
// since. A since this run never gave, another run's or one past its
// newest ask, is read from the oldest kept ask. When answer fails,
// nothing is marked.
func (in *inbox) read(since string, limit int, newest bool, label string, answer func(inboxRead) (string, error)) (string, error) {
	var after int64
	var nonce string
	if since != "" {
		var ok bool
		if after, nonce, ok = parseAskID(since); !ok {
			return "", fmt.Errorf("invalid arguments to page_state: since must be an ask ID as page_state returned it, such as i7-%s", strings.Repeat("0", 2*askNonceBytes))
		}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	out := inboxRead{rows: []askRow{}, dropped: in.dropped}
	if since != "" && (nonce != in.nonce || after > in.next) {
		out.foreign, after = true, 0
	}
	asks := in.asks
	if newest {
		asks = asks[max(0, len(asks)-limit):]
	} else {
		i, _ := slices.BinarySearchFunc(asks, after+1, func(a inboxAsk, n int64) int { return cmp.Compare(a.n, n) })
		asks = asks[i:min(len(asks), i+limit)]
		if since != "" && !out.foreign && len(in.asks) > 0 {
			out.lost = max(0, in.asks[0].n-(after+1))
		}
	}
	for _, a := range asks {
		row := askRow{ID: in.askID(a.n), Publication: a.publication, Node: a.node, Name: a.name,
			View: a.view, Digest: a.digest, At: stamp(a.at), ReadBy: cmp.Or(a.readBy, label), Question: a.question}
		if a.question != "" {
			row.QuestionSource = questionSource
		}
		out.rows = append(out.rows, row)
	}
	if u := in.user; u != nil {
		out.user = &userRow{Publication: u.publication, Selected: u.selected, Name: u.name, Focus: u.focus, At: stamp(u.at)}
	}
	text, err := answer(out)
	if err != nil {
		return "", err
	}
	// asks is a window of in.asks, so this marks the kept asks themselves.
	moved := false
	for i := range asks {
		if asks[i].readBy == "" {
			asks[i].readBy, moved = label, true
		}
	}
	if moved {
		in.changed()
	}
	return text, nil
}
