package perfmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

const openSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "A pprof profile (.pb.gz) inside the profiles root: relative to it, or an absolute path inside it."},
    "execlog": {"type": "string", "description": "Optional executions log (.execlog) of a GPU profile, inside the profiles root. It is checked against the profile; open reports whether it was accepted and why not."}
  },
  "required": ["path"],
  "additionalProperties": false
}`

const openDescription = "Open a perf-agent profile and return a handle for every other tool. " +
	"The handle names the evidence (the profile's SHA-256 and the executions log, if any) and stays valid while the server keeps it open. " +
	"The result lists the views (pass one as view), the default view, what the profile supports (executions, PC sampling, source roots, rendering), " +
	"the default view's quality and its sampling notes. Integers are decimal strings; an unavailable fact has a reason and no value."

func (s *Server) openTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name:        "open",
		Title:       "Open a profile",
		Description: openDescription,
		InputSchema: json.RawMessage(openSchema),
		Annotations: readOnly,
		Handler:     s.handleOpen,
	}
}

type openArgs struct {
	Path    string `json:"path"`
	Execlog string `json:"execlog"`
}

type viewInfo struct {
	View       string `json:"view"`
	SampleType string `json:"sample_type"`
	Unit       string `json:"unit"`
	Index      string `json:"index"`
}

type renderCapability struct {
	Enabled    bool   `json:"enabled"`
	OutputRoot string `json:"output_root,omitempty"`
}

type capabilities struct {
	Executions  profilemodel.Availability `json:"executions"`
	PCSampling  bool                      `json:"pc_sampling"`
	SourceRoots []string                  `json:"source_roots"`
	Render      renderCapability          `json:"render"`
}

type openResult struct {
	Handle        profilemodel.Handle `json:"handle"`
	ProfileDigest string              `json:"profile_digest"`
	ProfileName   string              `json:"profile_name"`
	CaptureID     string              `json:"capture_id"`
	Views         []viewInfo          `json:"views"`
	DefaultView   string              `json:"default_view"`
	Capabilities  capabilities        `json:"capabilities"`
	Quality       Quality             `json:"quality"`
	Notes         []string            `json:"notes"`
}

func (s *Server) handleOpen(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a openArgs
	if err := decodeArgs("open", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	if a.Path == "" {
		return mcpstdio.Result{}, fmt.Errorf("open needs path: a profile inside the profiles root %s", s.profiles.Name())
	}
	name, err := s.rel(s.profiles, a.Path)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	logName := ""
	if a.Execlog != "" {
		if logName, err = s.rel(s.profiles, a.Execlog); err != nil {
			return mcpstdio.Result{}, err
		}
	}
	e, err := s.store.open(name, logName)
	if err != nil {
		s.log.Warn("open refused", "path", a.Path, "execlog", a.Execlog, "err", err)
		return mcpstdio.Result{}, err
	}
	p := e.prof
	def := defaultView(p)
	_, v, err := s.store.view(p.Handle, def)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	notes, err := v.Notes()
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("cannot compute the sampling notes of %s: %v", def, err)
	}
	s.log.Info("opened", "path", name, "execlog", logName, "handle", p.Handle)

	out := openResult{
		Handle:        p.Handle,
		ProfileDigest: p.Digest.String(),
		ProfileName:   filepath.ToSlash(name),
		CaptureID:     p.CaptureID,
		DefaultView:   def,
		Capabilities: capabilities{
			Executions:  p.Executions,
			PCSampling:  pcSampling(p),
			SourceRoots: []string{},
		},
		Quality: wireQuality(v.Quality()),
		Notes:   nonNil(notes),
	}
	for i, st := range p.Source.SampleType {
		out.Views = append(out.Views, viewInfo{View: viewNames(p)[i], SampleType: st.Type, Unit: st.Unit, Index: wireInt(int64(i))})
	}
	for _, r := range s.sources {
		out.Capabilities.SourceRoots = append(out.Capabilities.SourceRoots, r.Name())
	}
	if s.output != nil {
		out.Capabilities.Render = renderCapability{Enabled: true, OutputRoot: s.output.Name()}
	}
	return s.respond("open", out)
}

// pcSampling reports whether any sample carries GPU PC sampling labels.
func pcSampling(p *profilemodel.Profile) bool {
	for _, smp := range p.Source.Sample {
		if _, ok := smp.Label["gpu_pc"]; ok {
			return true
		}
		if _, ok := smp.Label["gpu_src_status"]; ok {
			return true
		}
	}
	return false
}
