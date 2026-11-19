package perfmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/explain"
)

// capturePublish puts the committed capture m on the live page, as one
// more derivation under the engine done already holds, after derive made
// res: a facts-only page of the first mode's default view, made as
// page_explain makes one with explanations [], and with a compare a diff
// of that view with the baseline's, as page_diff makes one. Both are made
// by the session that started the job. With the job's show, the page is
// steered to the diff when there is one, else to the facts page; without
// it, nothing the page shows changes and both are only listed. It sets
// res's publications; an error leaves the capture as it is, and what was
// published before it stays.
func (s *Server) capturePublish(j *job, m manifest, res *captureResult) error {
	if _, err := s.ensureLive(); err != nil {
		return err
	}
	if len(res.Profiles) == 0 || res.Profiles[0].Open == nil {
		return errors.New("the capture has no open profile to publish")
	}
	pr := res.Profiles[0]
	o := pr.Open
	diff := s.captureDiffView(res.Compare, o)
	title := s.captureTitle(m, pr.Mode)
	c, err := s.checkCall(explainChecks, explainCall{
		Handle: o.Handle, View: o.DefaultView, Title: title, Explanations: json.RawMessage("[]"),
	})
	if err != nil {
		return err
	}
	if c.rejection != nil {
		// A view's facts alone carry no claim to fail.
		return errors.New("the facts page was rejected")
	}
	id := s.attempts.add(c.e.prof, o.DefaultView, "", outcomePublished, nil)
	s.recorded()
	p, err := s.publishExplained(c.e, c.v, o.DefaultView, c.r, title, id, j.by, j.req.Show && diff == nil)
	if err != nil {
		s.attempts.setOutcome(id, outcomeFailed)
		s.recorded()
		return s.pageError(err)
	}
	own := manifestFiles(m)
	p.files = addFiles(p.files, own...)
	res.Publication = p.id
	if diff == nil {
		return nil
	}
	base, next, err := s.diffSides(*diff)
	if err != nil {
		return err
	}
	d, err := s.compare(*diff, base, next)
	if err != nil {
		return err
	}
	title = s.cutTitle(fmt.Sprintf("%s-%s vs %s-%s · %s", captureKey(m.Series, m.ID), m.Label, res.Compare.Base.ID, res.Compare.Base.Label, pr.Mode))
	dp, err := s.publishDiff(d, base, next, title, "", j.by, j.req.Show)
	if err != nil {
		return s.pageError(err)
	}
	dp.files = addFiles(dp.files, own...)
	res.DiffPublication = dp.id
	return nil
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

// captureDiffView is the diff the capture's compare made of o's default
// view, as a query, or nil when it made none (the baseline has no such
// view, or there is no compare).
func (s *Server) captureDiffView(cr *compareResult, o *openResult) *diffQuery {
	if cr == nil {
		return nil
	}
	i := slices.IndexFunc(cr.Views, func(v compareView) bool {
		return v.Next.Arguments["new_handle"] == string(o.Handle) && v.Next.Arguments["new_view"] == o.DefaultView
	})
	if i < 0 {
		return nil
	}
	return &diffQuery{BaseHandle: cr.Base.Handle, BaseView: o.DefaultView, NewHandle: o.Handle, NewView: o.DefaultView}
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
