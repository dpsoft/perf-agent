package flamegraph

import (
	"fmt"
	"os"
	"path/filepath"
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
		StackOrder:  foldedstacks.RootFirst,
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
