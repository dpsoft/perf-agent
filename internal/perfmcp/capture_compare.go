package perfmcp

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// captureResult is a done capture as capture_wait answers it (§4.3). Its
// open and compare results are derived after the commit and live only in
// memory: Derived says "failed" (DeriveError says why; the capture stays
// committed) or, for an answer read back from a manifest, "recompute" with
// the calls that make them again. Handles is "reopen" when the handles are
// not this server's open ones. Publication and DiffPublication are the
// capture's pages on the live page, on a server that has one.
type captureResult struct {
	Capture         captureInfo     `json:"capture"`
	Profiles        []profileResult `json:"profiles"`
	Compare         *compareResult  `json:"compare,omitzero"`
	Evicted         []string        `json:"evicted,omitzero"`
	Notes           []string        `json:"notes,omitzero"`
	Cancel          string          `json:"cancel,omitzero"`
	Derived         string          `json:"derived,omitzero"`
	DeriveError     string          `json:"derive_error,omitzero"`
	Handles         string          `json:"handles,omitzero"`
	Recompute       *recompute      `json:"recompute,omitzero"`
	Publication     string          `json:"publication,omitzero"`
	DiffPublication string          `json:"diff_publication,omitzero"`
}

// captureInfo is the capture itself, as its manifest records it.
type captureInfo struct {
	ID                 string           `json:"id"`
	Capture            string           `json:"capture"`
	Series             string           `json:"series"`
	Label              string           `json:"label"`
	Manifest           string           `json:"manifest"`
	Settings           resultSettings   `json:"settings"`
	SettingsDigest     string           `json:"settings_digest"`
	Target             resultTarget     `json:"target,omitzero"`
	Modes              []string         `json:"modes"`
	RequestedDuration  string           `json:"requested_duration"`
	ProfiledDuration   string           `json:"profiled_duration,omitzero"`
	AttachLatency      string           `json:"attach_latency,omitzero"`
	SampleRate         int              `json:"sample_rate"`
	KernelStacks       bool             `json:"kernel_stacks"`
	StoppedBy          string           `json:"stopped_by,omitzero"`
	CompareWith        string           `json:"compare_with,omitzero"`
	Kept               bool             `json:"kept"`
	PerfAgent          perfAgentInfo    `json:"perf_agent,omitzero"`
	Survivors          []resultSurvivor `json:"survivors,omitzero"`
	Cleanup            string           `json:"cleanup,omitzero"`
	WorkloadOutputTail *outputTail      `json:"workload_output_tail,omitzero"`
}

// outputTail is the end of a launched command's output: the program's
// own, and untrusted.
type outputTail struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Untrusted bool   `json:"untrusted"`
}

// profileResult is one profile of the capture: its path in the profiles
// root, and the open tool's own result for it, or why it could not be
// opened.
type profileResult struct {
	Mode      string      `json:"mode"`
	Path      string      `json:"path"`
	Execlog   string      `json:"execlog,omitzero"`
	Open      *openResult `json:"open,omitzero"`
	OpenError string      `json:"open_error,omitzero"`
}

// compareResult is the capture diffed with its compare_with baseline.
type compareResult struct {
	Base          compareSide       `json:"base"`
	New           compareSide       `json:"new"`
	Comparability map[string]string `json:"comparability"`
	Views         []compareView     `json:"views"`
	Notes         []string          `json:"notes,omitzero"`
}

// compareSide is one side of a compare: the capture, and its handle.
type compareSide struct {
	ID     string              `json:"id"`
	Label  string              `json:"label"`
	Handle profilemodel.Handle `json:"handle,omitzero"`
}

// compareView is the diff tool's own result for one view, cut to
// Capture.CompareRows rows, and the call that pages the whole diff.
type compareView struct {
	diffResult
	Next toolCall `json:"next"`
}

// toolCall is a call an agent can make as it is. Bind names a call's
// result, which later calls use as "${<bind>.<field>}".
type toolCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Bind      string         `json:"bind,omitzero"`
}

// recompute is how to make a capture's derived results again from its
// files: the calls in order, and whether its baseline is "present",
// "none", or why it is gone.
type recompute struct {
	Steps    []toolCall `json:"steps"`
	Baseline string     `json:"baseline"`
}

// gpuEmptyNote is the note of a GPU profile with no samples.
const gpuEmptyNote = "the GPU profile has no samples: no kernel was launched through the adapter during the capture"

// capturesRel is the capture directory relative to the profiles root.
func (s *Server) capturesRel() string {
	if rel, err := s.rel(s.profiles, s.captureDirName()); err == nil {
		return filepath.Clean(rel)
	}
	return s.captureDirName()
}

// capturePath is the series file name as a path in the profiles root, as
// open takes it.
func (s *Server) capturePath(series, name string) string {
	return filepath.ToSlash(filepath.Join(s.capturesRel(), series, name))
}

// modesOf are m's modes, in canonical order.
func modesOf(m manifest) []capture.Mode {
	var modes []capture.Mode
	for _, x := range m.Settings.Modes {
		modes = append(modes, capture.Mode(x))
	}
	return capture.Settings{Modes: modes}.Canonical().Modes
}

// profilePaths are the profile, and for gpu the executions log, of m's
// mode as open takes them.
func (s *Server) profilePaths(m manifest, mode capture.Mode) (path, execlog string) {
	id, _ := parseID(m.ID)
	path = s.capturePath(m.Series, finalName(id, m.Label, capture.OutputFile(mode)))
	if mode == capture.ModeGPU {
		execlog = s.capturePath(m.Series, finalName(id, m.Label, capture.ExeclogFile))
	}
	return path, execlog
}

// captureInfo is m as a result shows it. A target that ended the
// capture says so, and when; a launched command's manifest also records
// its exit status, which a PID target's end does not give.
func (s *Server) captureInfo(m manifest) captureInfo {
	id, _ := parseID(m.ID)
	if m.Target.Exit == "" && (m.StoppedBy == capture.StoppedByTargetExit || m.StoppedBy == capture.StoppedByCommandExit) {
		m.Target.Exit = "exited after " + cmp.Or(m.ProfiledDuration, "an unknown time")
	}
	return captureInfo{
		ID: m.ID, Capture: captureKey(m.Series, m.ID), Series: m.Series, Label: m.Label, Manifest: s.capturePath(m.Series, manifestName(id, m.Label)),
		Settings: toResultSettings(m.Settings), SettingsDigest: m.SettingsDigest, Target: toResultTarget(m.Target), Modes: m.Settings.Modes,
		RequestedDuration: m.Settings.Duration, ProfiledDuration: m.ProfiledDuration, AttachLatency: m.AttachLatency,
		SampleRate: toResultSettings(m.Settings).SampleRate, KernelStacks: m.Settings.KernelStacks, StoppedBy: m.StoppedBy,
		CompareWith: m.CompareWith, Kept: m.Kept, PerfAgent: m.PerfAgent, Survivors: toResultSurvivors(m.Survivors), Cleanup: m.Cleanup,
	}
}

// derive makes the committed capture m's results under the engine: each
// profile opened with the existing open code (with its executions log for
// gpu) and, with a baseline, diffed at once with the baseline's profile of
// its mode by the existing diff code, so a compare needs only two open
// handles whatever its modes. What it opens stays pinned to j until the
// job lets go of it. A failure (a profile over the model limits, say) is
// reported as derived "failed" and never touches the files. The diffs are
// made from the pinned entries and views, so they need no more open
// handles than one: an open may evict a handle derive opened before.
//
// It also returns what the publication is made from: the first mode's
// pinned side and, when its view was diffed, the baseline's.
func (s *Server) derive(j *job, m manifest, base *manifest) (captureResult, derivation) {
	res := captureResult{Capture: s.captureInfo(m), Notes: slices.Clone(m.Notes)}
	if base != nil {
		res.Compare = &compareResult{
			Base:          compareSide{ID: base.ID, Label: base.Label},
			New:           compareSide{ID: m.ID, Label: m.Label},
			Comparability: comparability(m, *base),
			Views:         []compareView{},
		}
	}
	var failures []string
	d := derivation{base: base}
	for i, mode := range modesOf(m) {
		path, execlog := s.profilePaths(m, mode)
		pr := profileResult{Mode: string(mode), Path: path, Execlog: execlog}
		o, side, err := s.openPinned(j, path, execlog)
		if err != nil {
			pr.OpenError = err.Error()
			failures = append(failures, err.Error())
			res.Profiles = append(res.Profiles, pr)
			continue
		}
		pr.Open = o
		if i == 0 {
			d.first = &side
		}
		if mode == capture.ModeGPU && side.v.Total == 0 {
			res.Notes = append(res.Notes, gpuNote())
		}
		res.Profiles = append(res.Profiles, pr)
		if base != nil && len(failures) == 0 {
			bs, err := s.compareMode(j, res.Compare, *base, mode, o, side)
			if err != nil {
				failures = append(failures, err.Error())
			}
			if i == 0 {
				d.firstBase = bs
			}
		}
	}
	if len(failures) > 0 {
		res.Derived, res.DeriveError = derivedFailed, strings.Join(failures, "; ")
		res.Recompute = new(s.recomputeFor(m, base, ""))
		s.log.Warn("capture derivation failed; the capture stays committed", "job", j.id, "err", res.DeriveError)
	}
	return res, d
}

// derivation is what derive opened that a publication is made from, all
// pinned to the job: the first mode's side (its entry and default view)
// and, when that view was diffed with the baseline's, the baseline's side
// of that diff (nil otherwise); base is the baseline's manifest, nil
// without compare_with.
type derivation struct {
	first, firstBase *diffSide
	base             *manifest
}

// underived is m's result when nothing could be derived, for why.
func (s *Server) underived(m manifest, base *manifest, why string) captureResult {
	return captureResult{
		Capture: s.captureInfo(m), Notes: slices.Clone(m.Notes),
		Derived: derivedFailed, DeriveError: why, Recompute: new(s.recomputeFor(m, base, "")),
	}
}

// openPinned opens a capture's profile as open does and pins what it
// loaded to j (j is their holder in the pin ledger): the profile and its
// default view, returned as a side that stays valid after the store
// evicts the handle.
func (s *Server) openPinned(j *job, path, execlog string) (*openResult, diffSide, error) {
	if err := s.fault(faultBeforeOpen); err != nil {
		return nil, diffSide{}, fmt.Errorf("open %s: %w", path, err)
	}
	o, err := s.openResultFor(path, execlog)
	if err != nil {
		return nil, diffSide{}, err
	}
	// openResultFor has just opened the handle and built its default
	// view, so both are in the store.
	e, v, err := s.store.view(o.Handle, o.DefaultView)
	if err != nil {
		return nil, diffSide{}, err
	}
	s.pins.hold(j, e.prof, v)
	return &o, diffSide{e: e, v: v}, nil
}

// gpuNote is the empty GPU profile's note, with a hint when no NVIDIA
// driver is loaded.
func gpuNote() string {
	if _, err := os.Stat("/dev/nvidiactl"); err != nil {
		return gpuEmptyNote + "; /dev/nvidiactl is missing, so no NVIDIA driver is loaded"
	}
	return gpuEmptyNote
}

// compareMode opens the baseline's profile of mode and diffs no's default
// view (ns, pinned) with the baseline's view of that name, cut to
// CompareRows rows, into cr. It returns the baseline's side of the diff,
// pinned to j, or nil when the baseline has no such view.
func (s *Server) compareMode(j *job, cr *compareResult, base manifest, mode capture.Mode, no *openResult, ns diffSide) (*diffSide, error) {
	path, execlog := s.profilePaths(base, mode)
	bo, bs, err := s.openPinned(j, path, execlog)
	if err != nil {
		return nil, fmt.Errorf("baseline %s: %w", base.ID, err)
	}
	if cr.Base.Handle == "" {
		cr.Base.Handle, cr.New.Handle = bo.Handle, no.Handle
	}
	view := no.DefaultView
	if !slices.ContainsFunc(bo.Views, func(v viewInfo) bool { return v.View == view }) {
		cr.Notes = append(cr.Notes, fmt.Sprintf("the baseline's %s profile has no view %s to compare with", mode, view))
		return nil, nil
	}
	if view != bo.DefaultView {
		// The baseline was opened just now, so its handle is open: the
		// view of that name is built there and pinned too.
		_, v, err := s.store.view(bo.Handle, view)
		if err != nil {
			return nil, fmt.Errorf("baseline %s: %w", base.ID, err)
		}
		s.pins.hold(j, v)
		bs.v = v
	}
	if err := s.fault(faultBeforeDiff); err != nil {
		return nil, fmt.Errorf("diff %s: %w", view, err)
	}
	q := diffQuery{BaseHandle: bo.Handle, BaseView: view, NewHandle: no.Handle, NewView: view}
	dr, err := s.diffPageOf(q, bs, ns, s.lim.Capture.CompareRows, "")
	if err != nil {
		return nil, fmt.Errorf("diff %s: %w", view, err)
	}
	cr.Views = append(cr.Views, compareView{diffResult: dr, Next: toolCall{Tool: "diff", Arguments: map[string]any{
		"base_handle": string(bo.Handle), "base_view": view, "new_handle": string(no.Handle), "new_view": view,
	}}})
	return &bs, nil
}

// comparability reports what may differ between a capture and its
// baseline, for the agent to weigh; nothing is hidden or normalised.
func comparability(m, base manifest) map[string]string {
	c := map[string]string{"settings": "equal"}
	if m.SettingsDigest != base.SettingsDigest {
		c["settings"] = "differ"
	}
	switch b, n := base.Target.BuildID, m.Target.BuildID; {
	case b == "" || n == "":
		c["target_exe"] = "unknown"
	case b == n:
		c["target_exe"] = "same (build-id " + short(n) + ")"
	default:
		c["target_exe"] = "changed (build-id " + short(b) + " → " + short(n) + ")"
	}
	c["profiled_duration"] = cmp.Or(base.ProfiledDuration, "unknown") + " vs " + cmp.Or(m.ProfiledDuration, "unknown")
	c["attach_latency"] = cmp.Or(base.AttachLatency, "unknown") + " vs " + cmp.Or(m.AttachLatency, "unknown")
	return c
}

// short is a build-id cut to its first 12 characters.
func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

// recomputeFor is how to make m's results again from its files (§4.0): the
// baseline's opens first, then m's, each bound by name, then a diff per
// mode through the bindings, with CompareRows rows. A baseline that is
// gone (baseGone says why) is left out with its diffs; without
// compare_with there is none. With the live page the recipe publishes
// again as the capture did, under the same titles: the first mode's facts
// page, then, with the baseline, its diff.
func (s *Server) recomputeFor(m manifest, base *manifest, baseGone string) recompute {
	r := recompute{Steps: []toolCall{}, Baseline: "none"}
	modes := modesOf(m)
	opens := func(of manifest, side string) {
		for _, mode := range modes {
			path, execlog := s.profilePaths(of, mode)
			args := map[string]any{"path": path}
			if execlog != "" {
				args["execlog"] = execlog
			}
			r.Steps = append(r.Steps, toolCall{Tool: "open", Arguments: args, Bind: side + "_" + string(mode)})
		}
	}
	switch {
	case m.CompareWith == "":
	case baseGone != "":
		r.Baseline = baseGone
	case base == nil:
		r.Baseline = m.CompareWith + " was not read"
	default:
		r.Baseline = "present"
		opens(*base, "base")
	}
	opens(m, "new")
	if r.Baseline == "present" {
		for _, mode := range modes {
			b, n := "base_"+string(mode), "new_"+string(mode)
			r.Steps = append(r.Steps, toolCall{Tool: "diff", Arguments: map[string]any{
				"base_handle": "${" + b + ".handle}", "base_view": "${" + n + ".default_view}",
				"new_handle": "${" + n + ".handle}", "new_view": "${" + n + ".default_view}",
				"limit": s.lim.Capture.CompareRows,
			}})
		}
	}
	if s.live == nil || len(modes) == 0 {
		return r
	}
	b, n := "base_"+string(modes[0]), "new_"+string(modes[0])
	r.Steps = append(r.Steps, toolCall{Tool: "page_explain", Arguments: map[string]any{
		"handle": "${" + n + ".handle}", "view": "${" + n + ".default_view}", "explanations": []any{},
		"title": s.captureTitle(m, string(modes[0])),
	}})
	if r.Baseline == "present" {
		r.Steps = append(r.Steps, toolCall{Tool: "page_diff", Arguments: map[string]any{
			"base_handle": "${" + b + ".handle}", "base_view": "${" + n + ".default_view}",
			"new_handle": "${" + n + ".handle}", "new_view": "${" + n + ".default_view}",
			"title": s.diffTitle(m, *base, string(modes[0])),
		}})
	}
	return r
}

// withoutTool is r without its steps that call tool.
func withoutTool(r recompute, tool string) recompute {
	r.Steps = slices.DeleteFunc(slices.Clone(r.Steps), func(c toolCall) bool { return c.Tool == tool })
	return r
}
