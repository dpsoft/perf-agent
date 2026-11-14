package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// modelArgs are the parsed flags of the profile-model commands.
type modelArgs struct {
	in, out, title string
	sampleIdx      int
	execlog        string
	execlogBase    string
	facts          string
	listNodes      int
	diffBase       string
	set            map[string]bool // flags given explicitly
	rootFirst      bool
	folded         bool
}

// classify names a frame's domain for the facts; the model has no opinion on
// what a name means.
func classify(name, module string) string {
	return flamegraph.Classify(name, module).Info().Key
}

// runModel handles -facts, -list-nodes and -diff. The model reads pprof
// leaf-first only, so the flags that change how a profile is read or drawn
// are refused rather than silently ignored.
func runModel(stdout, stderr io.Writer, a modelArgs) int {
	modes := 0
	for _, f := range []string{"facts", "list-nodes", "diff"} {
		if a.set[f] {
			modes++
		}
	}
	switch {
	case modes > 1:
		return usageErr(stderr, "-facts, -list-nodes and -diff are mutually exclusive")
	case modes == 0:
		return usageErr(stderr, "-execlog and -execlog-base need -facts, -list-nodes or -diff")
	case a.folded:
		return usageErr(stderr, "-folded cannot be combined with -facts, -list-nodes or -diff")
	case a.rootFirst:
		return usageErr(stderr, "-stack-order root-first is not supported with -facts, -list-nodes or -diff: the profile model reads leaf-first profiles only")
	case a.set["gpu-telemetry"] || a.set["raw-vendor-frames"]:
		return usageErr(stderr, "-gpu-telemetry and -raw-vendor-frames do not apply to -facts, -list-nodes or -diff")
	case a.set["execlog-base"] && !a.set["diff"]:
		return usageErr(stderr, "-execlog-base needs -diff")
	case a.set["list-nodes"] && a.listNodes < 0:
		return usageErr(stderr, "-list-nodes wants a count of 0 or more")
	case !a.set["diff"] && (a.set["o"] || a.set["title"]):
		return usageErr(stderr, "-o and -title apply to -diff only")
	}

	// Load never fails on a sidecar's content, only reports it in the
	// facts; a sidecar named on the command line that cannot be opened is
	// a mistake in the command line, so it is refused before any work.
	for _, s := range []struct{ flag, path string }{{"execlog", a.execlog}, {"execlog-base", a.execlogBase}} {
		if s.path == "" {
			continue
		}
		if err := checkReadable(s.path); err != nil {
			return failf(stderr, "-%s: %v", s.flag, err)
		}
	}

	view, err := loadView(a.in, a.execlog, a.sampleIdx)
	if err != nil {
		return failf(stderr, "%v", err)
	}
	switch {
	case a.set["facts"]:
		return runFacts(stdout, stderr, view, a.facts)
	case a.set["list-nodes"]:
		return runListNodes(stdout, stderr, view, a.listNodes)
	}
	return runDiff(stdout, stderr, a, view)
}

// checkReadable reports why path cannot be read, or nil. Only a regular
// file is opened to check permission: opening and closing a pipe, such as
// a shell's process substitution, would cut off its writer.
func checkReadable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

func loadView(path, execlogPath string, sampleIdx int) (*profilemodel.View, error) {
	p, err := profilemodel.Load(path, execlogPath, profilemodel.DefaultLimits())
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	v, err := p.View(profilemodel.ViewOptions{SampleIndex: sampleIdx})
	if err != nil {
		return nil, fmt.Errorf("view %s: %w", path, err)
	}
	return v, nil
}

// namePath is a node's root-first names joined by ';', the form -facts reads.
func namePath(v *profilemodel.View, n *profilemodel.Node) string {
	path := v.Path(n)
	names := make([]string, len(path))
	for i, p := range path {
		names[i] = p.Function.Name
	}
	return strings.Join(names, ";")
}

func runFacts(stdout, stderr io.Writer, v *profilemodel.View, arg string) int {
	id := profilemodel.NodeID(arg)
	if _, ok := v.Node(id); !ok {
		matches := matchNamePath(v, arg)
		switch len(matches) {
		case 0:
			return failf(stderr, "-facts: no node with ID or name path %q", arg)
		case 1:
			id = matches[0].ID
		default:
			_, _ = fmt.Fprintf(stderr, "-facts: %q matches %d nodes; pass one of these IDs:\n", arg, len(matches))
			for _, n := range matches {
				_, _ = fmt.Fprintf(stderr, "  %s\t%s\n", n.ID, describeNode(n))
			}
			return 1
		}
	}
	facts, err := v.Facts(id, classify)
	if err != nil {
		return failf(stderr, "-facts: %v", err)
	}
	return writeJSON(stdout, stderr, facts, "  ")
}

// matchNamePath returns every node whose root-first function names equal the
// ';'-separated path, in tree order.
func matchNamePath(v *profilemodel.View, path string) []*profilemodel.Node {
	level := []*profilemodel.Node{v.Root}
	for name := range strings.SplitSeq(path, ";") {
		var next []*profilemodel.Node
		for _, n := range level {
			for _, c := range n.Children {
				if c.Function.Name == name {
					next = append(next, c)
				}
			}
		}
		level = next
	}
	return level
}

func describeNode(n *profilemodel.Node) string {
	mod := n.Function.Module.File
	if mod == "" {
		mod = "(no module)"
	}
	return fmt.Sprintf("%s in %s, cum %d", n.Function.Name, mod, n.Cum)
}

func runListNodes(stdout, stderr io.Writer, v *profilemodel.View, limit int) int {
	var nodes []*profilemodel.Node
	v.Walk(func(n *profilemodel.Node) bool {
		if n != v.Root {
			nodes = append(nodes, n)
		}
		return true
	})
	slices.SortFunc(nodes, func(a, b *profilemodel.Node) int {
		return cmp.Or(cmp.Compare(b.Cum, a.Cum), cmp.Compare(a.ID, b.ID))
	})
	nodes = nodes[:min(limit, len(nodes))]
	type row struct {
		ID   profilemodel.NodeID `json:"id"`
		Path string              `json:"path"`
		Cum  int64               `json:"cum"`
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, n := range nodes {
		if err := enc.Encode(row{n.ID, namePath(v, n), n.Cum}); err != nil {
			return failf(stderr, "-list-nodes: %v", err)
		}
	}
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		return failf(stderr, "-list-nodes: %v", err)
	}
	return 0
}

func writeJSON(stdout, stderr io.Writer, v any, indent string) int {
	b, err := json.MarshalIndent(v, "", indent)
	if err != nil {
		return failf(stderr, "encode JSON: %v", err)
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return failf(stderr, "write: %v", err)
	}
	return 0
}

func runDiff(stdout, stderr io.Writer, a modelArgs, newView *profilemodel.View) int {
	page := a.out
	if page == "" {
		page = a.in + ".diff.html"
	}
	data := strings.TrimSuffix(page, filepath.Ext(page)) + ".json"
	if filepath.Clean(data) == filepath.Clean(page) {
		return usageErr(stderr, "-o %q: the diff JSON would overwrite the page; use a .html name", page)
	}
	inputs := []string{a.in, a.diffBase, a.execlog, a.execlogBase}
	for _, dst := range []string{page, data} {
		if clash := clashingInput(dst, inputs); clash != "" {
			return failf(stderr, "refusing to overwrite input %s with output %s", clash, dst)
		}
	}

	baseView, err := loadView(a.diffBase, a.execlogBase, a.sampleIdx)
	if err != nil {
		return failf(stderr, "%v", err)
	}
	d, err := profilemodel.Compare(baseView, newView, profilemodel.WorkCounts{})
	if err != nil {
		return failf(stderr, "diff: %v", err)
	}
	title := a.title
	if title == "" {
		title = filepath.Base(a.in)
	}
	var html bytes.Buffer
	opts := flamegraph.DiffPageOptions{Title: title, BaseName: filepath.Base(a.diffBase), NewName: filepath.Base(a.in)}
	if err := flamegraph.WriteDiffPage(&html, d, opts); err != nil {
		return failf(stderr, "diff page: %v", err)
	}
	// Compact: the union is a tree as deep as the deepest stack, and
	// indenting it makes the file mostly whitespace.
	js, err := json.Marshal(d)
	if err != nil {
		return failf(stderr, "encode diff JSON: %v", err)
	}
	// Each file is replaced atomically; the pair is not. The JSON goes first
	// so a failed write never leaves a fresh page beside a stale JSON.
	if err := writeFileAtomic(data, append(js, '\n')); err != nil {
		return failf(stderr, "%v", err)
	}
	if err := writeFileAtomic(page, html.Bytes()); err != nil {
		return failf(stderr, "%v", err)
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s\nwrote %s\n", page, data)
	return 0
}

// clashingInput returns the input that dst names, by path or by identity of
// the existing file (a symlink or hard link to it), or "".
func clashingInput(dst string, inputs []string) string {
	abs, _ := filepath.Abs(dst)
	dstInfo, dstErr := os.Stat(dst)
	for _, in := range inputs {
		if in == "" {
			continue
		}
		if inAbs, _ := filepath.Abs(in); inAbs == abs {
			return in
		}
		if dstErr == nil {
			if info, err := os.Stat(in); err == nil && os.SameFile(info, dstInfo) {
				return in
			}
		}
	}
	return ""
}

// writeFileAtomic writes data to a temporary file beside path and renames it
// into place, so a failure never leaves a truncated output.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
