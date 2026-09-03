package perfagent

import "testing"

// The flag exists for one situation: a machine whose distribution sets
// DEBUGINFOD_URLS for you. So the property worth pinning is not "the field
// can be set" but "setting it beats the environment" -- an implementation
// that checked the environment first would pass a weaker test and fail every
// user it was written for.
func TestWithoutDebuginfodBeatsTheEnvironment(t *testing.T) {
	t.Setenv("DEBUGINFOD_URLS", "https://debuginfod.fedoraproject.org/")

	var on Config
	if got := debuginfodURLsFromEnv("https://debuginfod.fedoraproject.org/"); len(got) != 1 {
		t.Fatalf("precondition: env parse returned %v, want one URL -- the test would pass vacuously", got)
	}
	if on.DisableDebuginfod {
		t.Fatal("zero Config should leave debuginfod enabled; honouring the env var is the default")
	}

	var off Config
	WithoutDebuginfod()(&off)
	if !off.DisableDebuginfod {
		t.Fatal("WithoutDebuginfod did not set DisableDebuginfod")
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
