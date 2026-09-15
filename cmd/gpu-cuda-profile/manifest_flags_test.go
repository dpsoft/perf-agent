package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The example manifests invoke this binary by name and pass it flags. Until
// this test existed, nothing checked that those flags were real -- and the
// sidecar example had been passing --gpu, --gpu-shim and --gpu-output since
// it was written, none of which this command has ever defined.
//
// That is worse than a typo in a comment. A manifest reads as authoritative:
// it is the thing an operator copies, and every flag in it looks like a
// feature. The failure mode is an afternoon lost to `kubectl logs` on a pod
// that died at flag parsing.
//
// This test is deliberately mechanical -- it re-derives the flag set from
// defineFlags rather than restating it -- so a flag renamed in the code
// fails here instead of rotting in the YAML.

// manifestArgs pulls the args of every container and initContainer out of a
// manifest, keyed by container name so a failure says which one is wrong.
func manifestArgs(t *testing.T, path string) map[string][]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	out := map[string][]string{}
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var doc map[string]any
		switch err := dec.Decode(&doc); {
		case err == io.EOF:
			return out
		case err != nil:
			t.Fatalf("parse %s: %v", path, err)
		case doc == nil:
			continue
		}
		collectContainers(doc, out)
	}
}

// collectContainers walks an arbitrary manifest shape looking for container
// lists. Walking rather than indexing a fixed path keeps this working for a
// bare Pod and for a DaemonSet's nested template alike.
func collectContainers(node any, out map[string][]string) {
	switch v := node.(type) {
	case map[string]any:
		for key, val := range v {
			if key == "containers" || key == "initContainers" {
				list, ok := val.([]any)
				if !ok {
					continue
				}
				for _, item := range list {
					c, ok := item.(map[string]any)
					if !ok {
						continue
					}
					name, _ := c["name"].(string)
					rawArgs, ok := c["args"].([]any)
					if !ok {
						continue
					}
					var args []string
					for _, a := range rawArgs {
						if s, ok := a.(string); ok {
							args = append(args, s)
						}
					}
					if len(args) > 0 {
						out[name] = args
					}
				}
			}
			collectContainers(val, out)
		}
	case []any:
		for _, item := range v {
			collectContainers(item, out)
		}
	}
}

// flagNameOf extracts the flag name from one arg, or "" when the arg is not
// a flag (a subcommand, or the value half of "-flag value").
func flagNameOf(arg string) string {
	if !strings.HasPrefix(arg, "-") {
		return ""
	}
	name := strings.TrimLeft(arg, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name
}

// definedFlags is the real flag set, taken from defineFlags rather than
// listed here, so renaming a flag in the code fails this test.
func definedFlags(t *testing.T) map[string]bool {
	t.Helper()
	fs := flag.NewFlagSet("manifest-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	defineFlags(fs)
	got := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { got[f.Name] = true })
	return got
}

func TestExampleManifestsPassFlagsThisCommandActuallyHas(t *testing.T) {
	defined := definedFlags(t)

	// Only containers running THIS image are checked. The app container and
	// the shim installer run other images with their own arguments.
	const agentImage = "ghcr.io/dpsoft/perf-agent"

	for _, manifest := range []string{
		"../../examples/kubernetes/pytorch-gpu-profile.yaml",
		"../../examples/kubernetes/gpu-collector-daemonset.yaml",
	} {
		body, err := os.ReadFile(manifest)
		if err != nil {
			t.Fatalf("read %s: %v", manifest, err)
		}
		if !strings.Contains(string(body), agentImage) {
			t.Fatalf("%s no longer names %s; this test is checking the wrong file",
				filepath.Base(manifest), agentImage)
		}

		args := manifestArgs(t, manifest)
		if len(args) == 0 {
			t.Fatalf("%s: found no container args at all. Either the manifest changed "+
				"shape or the walker is broken -- and a walker that finds nothing would "+
				"pass this test forever", filepath.Base(manifest))
		}

		checked := 0
		for container, list := range args {
			// The agent container is the one whose args reach this binary.
			// Identify it by the flags it passes rather than by name, since
			// names differ between the two manifests.
			if !strings.Contains(strings.Join(list, " "), "-out") &&
				!strings.Contains(strings.Join(list, " "), "-shim") {
				continue
			}
			checked++
			for _, arg := range list {
				name := flagNameOf(arg)
				if name == "" {
					continue
				}
				if !defined[name] {
					t.Errorf("%s: container %q passes -%s, which %s does not define. "+
						"A manifest is what an operator copies, so every flag in it reads "+
						"as a feature; this one fails at flag parsing.",
						filepath.Base(manifest), container, name, "gpu-cuda-profile")
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s: no container passed -out or -shim, so nothing was checked",
				filepath.Base(manifest))
		}
	}
}
