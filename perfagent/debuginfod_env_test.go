package perfagent

import (
	"reflect"
	"testing"
)

// Issue #109. DEBUGINFOD_URLS is whitespace-separated but is not only URLs:
// elfutils' debuginfod-client also accepts policy tokens in the same list, and
// Fedora ships exactly that by default. Treating every field as a server hands
// non-servers to the fetcher.
func TestDebuginfodURLsFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string
	}{
		{
			// The value that is actually set on Fedora, and the one that
			// motivated this: two of the three fields are not servers.
			name: "fedora default carries ima policy tokens",
			env:  "ima:enforcing https://debuginfod.fedoraproject.org/ ima:ignore",
			want: []string{"https://debuginfod.fedoraproject.org/"},
		},
		{
			name: "plain single url",
			env:  "https://debuginfod.example/",
			want: []string{"https://debuginfod.example/"},
		},
		{
			name: "several servers keep their order",
			env:  "https://a.example/ http://b.example/",
			want: []string{"https://a.example/", "http://b.example/"},
		},
		{
			name: "unset",
			env:  "",
			want: nil,
		},
		{
			name: "only tokens yields nothing, so the local symbolizer is used",
			env:  "ima:enforcing ima:ignore",
			want: nil,
		},
		{
			// A scheme with no host is not a server, a bare path has no
			// scheme at all, and ftp is not something this agent fetches
			// from. None of these may reach the fetcher.
			name: "non-http schemes and hostless URLs are refused",
			env:  "https:// file:///srv/debug ftp://x.example/ /just/a/path",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := debuginfodURLsFromEnv(tc.env)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("debuginfodURLsFromEnv(%q) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}
