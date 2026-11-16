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
// not this server's open ones.
type captureResult struct {
	Capture     captureInfo     `json:"capture"`
	Profiles    []profileResult `json:"profiles"`
	Compare     *compareResult  `json:"compare,omitzero"`
	Evicted     []string        `json:"evicted,omitzero"`
	Notes       []string        `json:"notes,omitzero"`
	Cancel      string          `json:"cancel,omitzero"`
	Derived     string          `json:"derived,omitzero"`
	DeriveError string          `json:"derive_error,omitzero"`
	Handles     string          `json:"handles,omitzero"`
	Recompute   *recompute      `json:"recompute,omitzero"`
}

// captureInfo is the capture itself, as its manifest records it.
type captureInfo struct {
	ID                 string         `json:"id"`
	Series             string         `json:"series"`
	Label              string         `json:"label"`
	Manifest           string         `json:"manifest"`
	Settings           wireSettings   `json:"settings"`
	SettingsDigest     string         `json:"settings_digest"`
	Target             manifestTarget `json:"target,omitzero"`
	Modes              []string       `json:"modes"`
	RequestedDuration  string         `json:"requested_duration"`
	ProfiledDuration   string         `json:"profiled_duration,omitzero"`
	AttachLatency      string         `json:"attach_latency,omitzero"`
	SampleRate         string         `json:"sample_rate"`
	KernelStacks       bool           `json:"kernel_stacks"`
	StoppedBy          string         `json:"stopped_by,omitzero"`
	CompareWith        string         `json:"compare_with,omitzero"`
	Kept               bool           `json:"kept"`
	PerfAgent          perfAgentInfo  `json:"perf_agent,omitzero"`
	Survivors          []wireSurvivor `json:"survivors,omitzero"`
	Cleanup            string         `json:"cleanup,omitzero"`
	WorkloadOutputTail *outputTail    `json:"workload_output_tail,omitzero"`
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

// captureInfo is m as a result shows it.
func (s *Server) captureInfo(m manifest) captureInfo {
	id, _ := parseID(m.ID)
	return captureInfo{
		ID: m.ID, Series: m.Series, Label: m.Label, Manifest: s.capturePath(m.Series, manifestName(id, m.Label)),
		Settings: m.Settings, SettingsDigest: m.SettingsDigest, Target: m.Target, Modes: m.Settings.Modes,
		RequestedDuration: m.Settings.Duration, ProfiledDuration: m.ProfiledDuration, AttachLatency: m.AttachLatency,
		SampleRate: m.Settings.SampleRate, KernelStacks: m.Settings.KernelStacks, StoppedBy: m.StoppedBy,
		CompareWith: m.CompareWith, Kept: m.Kept, PerfAgent: m.PerfAgent, Survivors: m.Survivors, Cleanup: m.Cleanup,
	}
}

// derive makes the committed capture m's results under the engine: each
// profile opened with the existing open code (with its executions log for
// gpu), and with a baseline, each view diffed with the existing diff code.
// The profiles stay pinned to j until the job lets go of them. A failure
// (a profile over the model limits, say) is reported as derived "failed"
// and never touches the files.
func (s *Server) derive(j *job, m manifest, base *manifest) captureResult {
	res := captureResult{Capture: s.captureInfo(m), Notes: slices.Clone(m.Notes)}
	var failures []string
	opened, errs := s.openAll(j, m)
	for _, mode := range modesOf(m) {
		path, execlog := s.profilePaths(m, mode)
		pr := profileResult{Mode: string(mode), Path: path, Execlog: execlog, Open: opened[mode]}
		if err := errs[mode]; err != nil {
			pr.OpenError = err.Error()
			failures = append(failures, err.Error())
		} else if mode == capture.ModeGPU && s.viewTotal(pr.Open) == 0 {
			res.Notes = append(res.Notes, gpuNote())
		}
		res.Profiles = append(res.Profiles, pr)
	}
	if base != nil && len(failures) == 0 {
		cr, err := s.compareWith(j, m, *base, opened)
		if err != nil {
			failures = append(failures, err.Error())
		}
		res.Compare = cr
	}
	if len(failures) > 0 {
		res.Derived, res.DeriveError = derivedFailed, strings.Join(failures, "; ")
		res.Recompute = new(s.recomputeFor(m, base, ""))
		s.log.Warn("capture derivation failed; the capture stays committed", "job", j.id, "err", res.DeriveError)
	}
	return res
}

// underived is m's result when nothing could be derived, for why.
func (s *Server) underived(m manifest, base *manifest, why string) captureResult {
	return captureResult{
		Capture: s.captureInfo(m), Notes: slices.Clone(m.Notes),
		Derived: derivedFailed, DeriveError: why, Recompute: new(s.recomputeFor(m, base, "")),
	}
}

// openAll opens each of m's profiles and pins what it loaded to j.
func (s *Server) openAll(j *job, m manifest) (map[capture.Mode]*openResult, map[capture.Mode]error) {
	opened, errs := map[capture.Mode]*openResult{}, map[capture.Mode]error{}
	for _, mode := range modesOf(m) {
		path, execlog := s.profilePaths(m, mode)
		if err := s.fault(faultBeforeOpen); err != nil {
			errs[mode] = fmt.Errorf("open %s: %w", path, err)
			continue
		}
		o, err := s.openResultFor(path, execlog)
		if err != nil {
			errs[mode] = err
			continue
		}
		opened[mode] = &o
		if e, ok := s.store.entries[o.Handle]; ok {
			objs := []retained{e.prof}
			if v, ok := e.views[o.DefaultView]; ok {
				objs = append(objs, v)
			}
			s.pins.hold(j, objs...)
			j.pins = append(j.pins, o.Handle)
		}
	}
	return opened, errs
}

// viewTotal is the total of o's default view, -1 when it cannot be read.
func (s *Server) viewTotal(o *openResult) int64 {
	if o == nil {
		return -1
	}
	_, v, err := s.store.view(o.Handle, o.DefaultView)
	if err != nil {
		return -1
	}
	return v.Total
}

// gpuNote is the empty GPU profile's note, with a hint when no NVIDIA
// driver is loaded.
func gpuNote() string {
	if _, err := os.Stat("/dev/nvidiactl"); err != nil {
		return gpuEmptyNote + "; /dev/nvidiactl is missing, so no NVIDIA driver is loaded"
	}
	return gpuEmptyNote
}

// compareWith opens the baseline's profiles and diffs each of m's default
// views with the baseline's view of that name, cut to CompareRows rows.
func (s *Server) compareWith(j *job, m, base manifest, opened map[capture.Mode]*openResult) (*compareResult, error) {
	cr := &compareResult{
		Base:          compareSide{ID: base.ID, Label: base.Label},
		New:           compareSide{ID: m.ID, Label: m.Label},
		Comparability: comparability(m, base),
		Views:         []compareView{},
	}
	baseOpened, errs := s.openAll(j, base)
	for _, mode := range modesOf(m) {
		if err := errs[mode]; err != nil {
			return cr, fmt.Errorf("baseline %s: %w", base.ID, err)
		}
		bo, no := baseOpened[mode], opened[mode]
		if bo == nil || no == nil {
			cr.Notes = append(cr.Notes, fmt.Sprintf("the baseline has no %s profile to compare with", mode))
			continue
		}
		if cr.Base.Handle == "" {
			cr.Base.Handle, cr.New.Handle = bo.Handle, no.Handle
		}
		view := no.DefaultView
		if !slices.ContainsFunc(bo.Views, func(v viewInfo) bool { return v.View == view }) {
			cr.Notes = append(cr.Notes, fmt.Sprintf("the baseline's %s profile has no view %s to compare with", mode, view))
			continue
		}
		if err := s.fault(faultBeforeDiff); err != nil {
			return cr, fmt.Errorf("diff %s: %w", view, err)
		}
		q := diffQuery{BaseHandle: bo.Handle, BaseView: view, NewHandle: no.Handle, NewView: view}
		dr, err := s.diffPage(q, s.lim.Capture.CompareRows, "")
		if err != nil {
			return cr, fmt.Errorf("diff %s: %w", view, err)
		}
		cr.Views = append(cr.Views, compareView{diffResult: dr, Next: toolCall{Tool: "diff", Arguments: map[string]any{
			"base_handle": string(bo.Handle), "base_view": view, "new_handle": string(no.Handle), "new_view": view,
		}}})
	}
	return cr, nil
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
// compare_with there is none.
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
	if r.Baseline != "present" {
		return r
	}
	for _, mode := range modes {
		b, n := "base_"+string(mode), "new_"+string(mode)
		r.Steps = append(r.Steps, toolCall{Tool: "diff", Arguments: map[string]any{
			"base_handle": "${" + b + ".handle}", "base_view": "${" + n + ".default_view}",
			"new_handle": "${" + n + ".handle}", "new_view": "${" + n + ".default_view}",
			"limit": s.lim.Capture.CompareRows,
		}})
	}
	return r
}
