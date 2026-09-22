package flamegraph

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

// FromProfileFile reads a pprof profile (gzipped or not) and writes a
// self-contained HTML flame graph.
//
// It reads the profile back off disk rather than tapping the builder in
// memory. That is deliberate: it means the page is a rendering of the file
// the user was actually given, so a page that looks wrong is evidence about
// the artifact rather than about a second, parallel code path.
//
// The returned Result is the folding summary — sample counts, unsymbolized
// frame counts, warnings — so a caller can echo the honest numbers to the
// terminal alongside the file it just wrote. It is returned even when the
// profile is degenerate; that case is not an error, and the page says so.
func FromProfileFile(profilePath, htmlPath string, opts Options) (*foldedstacks.Result, error) {
	f, err := os.Open(profilePath)
	if err != nil {
		return nil, fmt.Errorf("open profile: %w", err)
	}
	defer func() { _ = f.Close() }()

	p, err := profile.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse profile %s: %w", profilePath, err)
	}

	res, err := foldedstacks.Fold(p, foldedstacks.Options{
		SampleIndex: -1,
		StackOrder:  opts.StackOrder,
		// The RENDERER opts in, and only the renderer. Folding is also how
		// `flamegraph -folded` produces text for other tools, and quietly
		// changing what those stacks contain would alter somebody else's
		// pipeline. A picture is the one consumer that is read by a person,
		// and the one where three unreadable rows cost more than they carry.
		CollapseVendorRuns: !opts.RawVendorFrames,
	})
	if err != nil {
		return nil, fmt.Errorf("fold %s: %w", profilePath, err)
	}

	if opts.Title == "" {
		opts.Title = filepath.Base(profilePath)
	}
	if opts.Subtitle == "" {
		opts.Subtitle = subtitleFor(profilePath, p)
	}

	out, err := os.Create(htmlPath)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", htmlPath, err)
	}
	if err := RenderHTML(out, res, opts); err != nil {
		_ = out.Close()
		return nil, fmt.Errorf("render %s: %w", htmlPath, err)
	}
	if err := out.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", htmlPath, err)
	}
	return res, nil
}

func subtitleFor(profilePath string, p *profile.Profile) string {
	s := profilePath
	if p.TimeNanos > 0 {
		s += "  ·  collected " + time.Unix(0, p.TimeNanos).Format(time.RFC3339)
	}
	if p.DurationNanos > 0 {
		s += "  ·  over " + time.Duration(p.DurationNanos).String()
	}
	return s
}

// FuseFile is one input to FuseProfileFiles.
type FuseFile struct {
	// Path is the pprof profile to read (gzipped or not).
	Path string
	// Label names the synthetic root this file's stacks hang under.
	// Defaults to the file's base name.
	Label string
	// StackOrder is how THIS file stores Sample.Location. It is per-file
	// because the inputs need not agree: until issue #155 is fixed, a
	// default CPU capture is leaf-first while a GPU capture is root-first,
	// so no single setting can read both.
	StackOrder foldedstacks.StackOrder
}

// FuseProfileFiles renders several pprof profiles as one flame graph, each
// hanging under its own labelled root.
//
// It answers the question two separate collectors leave open: CPU and GPU,
// or on-CPU and off-CPU, are measured into different files, and a reader
// wants one picture. The alternative — merging the pprof profiles before
// rendering — is a trap. Rebuilding frames without carrying Mapping through
// silently disables vendor-run collapsing, because isVendorModule keys on
// the module; and guessing a shared stack order draws a plausible graph
// upside down. Both failures produce a picture and no error.
//
// Fusing happens after the fold instead, where Frames are already normalized
// root-first and Modules already resolved, so neither mistake is available
// to make. See foldedstacks.Fuse.
func FuseProfileFiles(inputs []FuseFile, htmlPath string, opts Options) (*foldedstacks.Result, error) {
	if len(inputs) < 2 {
		return nil, fmt.Errorf("flamegraph: fusing needs at least 2 profiles, got %d", len(inputs))
	}

	folded := make([]foldedstacks.FuseInput, 0, len(inputs))
	for _, in := range inputs {
		f, err := os.Open(in.Path)
		if err != nil {
			return nil, fmt.Errorf("open profile: %w", err)
		}
		p, err := profile.Parse(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("parse profile %s: %w", in.Path, err)
		}

		res, err := foldedstacks.Fold(p, foldedstacks.Options{
			SampleIndex:        -1,
			StackOrder:         in.StackOrder,
			CollapseVendorRuns: !opts.RawVendorFrames,
		})
		if err != nil {
			return nil, fmt.Errorf("fold %s: %w", in.Path, err)
		}

		label := in.Label
		if label == "" {
			label = filepath.Base(in.Path)
		}
		folded = append(folded, foldedstacks.FuseInput{Result: res, Label: label})
	}

	fused, err := foldedstacks.Fuse(folded)
	if err != nil {
		return nil, err
	}

	if opts.Title == "" {
		opts.Title = "Fused flame graph"
	}
	if opts.Subtitle == "" {
		paths := make([]string, 0, len(inputs))
		for _, in := range inputs {
			paths = append(paths, filepath.Base(in.Path))
		}
		opts.Subtitle = "fused from " + strings.Join(paths, " + ")
	}

	out, err := os.Create(htmlPath)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", htmlPath, err)
	}
	if err := RenderHTML(out, fused, opts); err != nil {
		_ = out.Close()
		return nil, fmt.Errorf("render %s: %w", htmlPath, err)
	}
	if err := out.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", htmlPath, err)
	}
	return fused, nil
}
