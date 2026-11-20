package perfmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/explain"
)

// capturePublish puts the committed capture m on the live page, as one
// more derivation under the engine done already holds, after derive made
// res and d: a facts-only page of the first mode's default view, checked
// as page_explain checks explanations [], and, when that view was diffed
// with the baseline's, a diff of the two, as page_diff makes one. Both are
// built from the sides derive pinned to the job, so a handle the store
// evicted meanwhile does not matter. Both are made by the session that
// started the job. The facts page is the server's, not an agent's
// attempt: it records none.
//
// With the job's show, the page is steered to the diff when there is one,
// else to the facts page; without it, nothing the page shows changes and
// both are only listed. Each publication keeps the files of the captures
// it shows from retention. It sets res's publications. When the facts
// page was published and the diff was not, the facts page stays (and is
// steered to with show), and the error says so.
func (s *Server) capturePublish(j *job, m manifest, res *captureResult, d derivation) error {
	if _, err := s.ensureLive(); err != nil {
		return err
	}
	if d.first == nil || len(res.Profiles) == 0 {
		return errors.New("the capture has no open profile to publish")
	}
	mode := res.Profiles[0].Mode
	view := res.Profiles[0].Open.DefaultView
	exps, _, err := explain.Decode(json.RawMessage("[]"), s.lim.Explain)
	if err != nil {
		return err
	}
	r := explain.Check(d.first.v, exps, s.lim.Explain)
	if !r.OK {
		// A view's facts alone carry no claim to fail.
		return errors.New("the facts page was rejected")
	}
	diff := d.firstBase != nil && d.base != nil
	// This publishes without checkCall, and so without its title check:
	// captureTitle runs explain.CheckTitle itself and leaves out the comm
	// it refuses; the rest of the title is the server's own text, cut to
	// Explain.MaxTitleBytes.
	p, err := s.publishExplained(d.first.e, d.first.v, view, r, s.captureTitle(m, mode), "", j.by, j.req.Show && !diff)
	if err != nil {
		return s.pageError(err)
	}
	own := manifestFiles(m)
	p.files = addFiles(p.files, own...)
	res.Publication = p.id
	if !diff {
		return nil
	}
	if err := s.publishCaptureDiff(j, m, res, d, view, mode, own); err != nil {
		shown := ""
		if j.req.Show {
			if serr := s.live.show(p.id, p.firstSelected(), false, j.by); serr == nil {
				s.commit()
				shown = " and shown"
			}
		}
		return fmt.Errorf("the facts page %s is kept%s, but the diff was not published: %w", p.id, shown, err)
	}
	return nil
}

// publishCaptureDiff publishes the diff of the first mode's view, which
// derive made, with the baseline's, steered to with the job's show.
func (s *Server) publishCaptureDiff(j *job, m manifest, res *captureResult, d derivation, view, mode string, own []fileKey) error {
	if s.beforeCaptureDiff != nil {
		if err := s.beforeCaptureDiff(); err != nil {
			return err
		}
	}
	q := diffQuery{BaseHandle: d.firstBase.e.prof.Handle, BaseView: view, NewHandle: d.first.e.prof.Handle, NewView: view}
	dd, err := s.compare(q, *d.firstBase, *d.first)
	if err != nil {
		return err
	}
	dp, err := s.publishDiff(dd, *d.firstBase, *d.first, s.diffTitle(m, *d.base, mode), "", j.by, j.req.Show)
	if err != nil {
		return s.pageError(err)
	}
	dp.files = addFiles(dp.files, own...)
	dp.files = addFiles(dp.files, manifestFiles(*d.base)...)
	res.DiffPublication = dp.id
	return nil
}

// diffTitle is the title of capture m's diff with its baseline base, of
// mode: "<series>/NNN-<label> vs <BBB>-<base label> · <mode>", cut to
// Explain.MaxTitleBytes. Series, ids and labels are names of
// [A-Za-z0-9._-], so it is always a title page_diff takes.
func (s *Server) diffTitle(m, base manifest, mode string) string {
	return s.cutTitle(fmt.Sprintf("%s-%s vs %s-%s · %s", captureKey(m.Series, m.ID), m.Label, base.ID, base.Label, mode))
}

// manifestFiles is the files m records, by device and inode.
func manifestFiles(m manifest) []fileKey {
	out := make([]fileKey, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, fileKey{f.Dev, f.Ino})
	}
	return out
}

// pageError is a publication's error as a capture's derive_error says it:
// a page over MaxPageBytes says which limit to raise.
func (s *Server) pageError(err error) error {
	if _, ok := errors.AsType[*modelError](err); ok {
		return fmt.Errorf("the page cannot be built within this server's MaxPageBytes=%d: %v; %s", s.lim.MaxPageBytes, err, s.raiseAdvice("MaxPageBytes"))
	}
	return err
}

// captureTitle is the title of capture m's facts page of mode:
// "<series>/NNN-<label> · <comm> (<pid>) · <mode> · <profiled duration>",
// without what the manifest does not record, cut to Explain.MaxTitleBytes.
// A comm a title cannot hold (one that reads as a placeholder) is left
// out.
func (s *Server) captureTitle(m manifest, mode string) string {
	head := captureKey(m.Series, m.ID) + "-" + m.Label
	target := m.Target.Comm
	if target == "" {
		target = m.Target.Exe
	}
	if m.Target.PID != "" {
		target = strings.TrimSpace(target + " (" + m.Target.PID + ")")
	}
	build := func(target string) string {
		parts := []string{head}
		for _, p := range []string{target, mode, m.ProfiledDuration} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return s.cutTitle(strings.Join(parts, " · "))
	}
	title := build(target)
	if explain.CheckTitle(title, s.lim.Explain) != nil {
		title = build("")
	}
	return title
}

// cutTitle is title cut to Explain.MaxTitleBytes, on a rune boundary.
func (s *Server) cutTitle(title string) string {
	n := s.lim.Explain.MaxTitleBytes
	if len(title) <= n {
		return title
	}
	for n > 0 && !utf8.RuneStart(title[n]) {
		n--
	}
	return title[:n]
}
