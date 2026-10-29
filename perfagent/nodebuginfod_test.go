package perfagent

import (
	"slices"
	"testing"
)

// Which servers a capture fetches from, for every way of asking. Issue #111.
//
// This drives debuginfodServers, the function chooseSymbolizer calls, rather
// than checking that an option sets a field. The test it replaces did only
// that, and passed for as long as --no-debuginfod was a no-op: the field was
// set, and nothing ever read it.
func TestDebuginfodServers(t *testing.T) {
	const fedora = "ima:enforcing https://debuginfod.fedoraproject.org/ ima:ignore"
	envURL := []string{"https://debuginfod.fedoraproject.org/"}
	mine := []string{"http://localhost:8002"}
	if got := debuginfodURLsFromEnv(fedora); !slices.Equal(got, envURL) {
		t.Fatalf("precondition: env parse returned %v, want %v -- cases below would pass vacuously", got, envURL)
	}

	cases := []struct {
		name        string
		opts        []Option
		env         string
		use, ignore []string
	}{
		{name: "environment alone is not enough", env: fedora, ignore: envURL},
		{name: "--debuginfod uses the environment", env: fedora, opts: []Option{WithDebuginfodFromEnv()}, use: envURL},
		{name: "--debuginfod-url needs no environment", opts: []Option{WithDebuginfodURL(mine[0])}, use: mine},
		{name: "--debuginfod-url beats the environment", env: fedora,
			opts: []Option{WithDebuginfodFromEnv(), WithDebuginfodURL(mine[0])}, use: mine},
		{name: "--no-debuginfod beats the environment", env: fedora,
			opts: []Option{WithDebuginfodFromEnv(), WithoutDebuginfod()}},
		{name: "--no-debuginfod beats --debuginfod-url",
			opts: []Option{WithDebuginfodURL(mine[0]), WithoutDebuginfod()}},
		{name: "--debuginfod with nothing in the environment", opts: []Option{WithDebuginfodFromEnv()}},
		{name: "nothing at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			for _, o := range tc.opts {
				o(&c)
			}
			use, ignore := debuginfodServers(&c, tc.env)
			if !slices.Equal(use, tc.use) || !slices.Equal(ignore, tc.ignore) {
				t.Errorf("use=%v ignore=%v, want use=%v ignore=%v", use, ignore, tc.use, tc.ignore)
			}
		})
	}
}

// An empty --debuginfod-url stays an error rather than meaning "disable":
// a script writing --debuginfod-url="$URL" with URL unset should be told,
// not silently downgraded to local symbols.
func TestEmptyDebuginfodURLIsStillAnError(t *testing.T) {
	var c Config
	WithDebuginfodURL("")(&c)
	if len(c.DebuginfodURLs) != 1 || c.DebuginfodURLs[0] != "" {
		t.Fatalf("option layer should not filter; flag layer rejects empty. got %v", c.DebuginfodURLs)
	}
}
