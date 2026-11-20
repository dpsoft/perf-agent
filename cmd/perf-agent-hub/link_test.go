package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/mcpflags/binarytest"
)

// The hub is the one binary with a network. The source scan
// (TestNoClientNetworkingInTheServer) reads its packages; these tests
// check what it imports and links.

// networkingPackages are the packages that open or accept connections.
// net/url and net/netip only parse addresses, and stay allowed.
var networkingPackages = []string{"net", "net/http", "crypto/tls", "net/textproto", "log/syslog"}

// The hub's packages that serve: the loopback listener and front door,
// the page's routes and inbox, and MCP over Streamable HTTP.
const (
	livehttpPkg = "github.com/dpsoft/perf-agent/internal/livehttp"
	perfpagePkg = "github.com/dpsoft/perf-agent/internal/perfpage"
	mcphttpPkg  = "github.com/dpsoft/perf-agent/internal/mcphttp"
	hubPkg      = "github.com/dpsoft/perf-agent/cmd/perf-agent-hub"
)

// networkImports are the networking imports each package may have; no
// other package of the hub imports one. Only livehttp binds and wraps the
// listener's connections; the main package names net's listener and
// connection types for its connection cap, and joins the address; the
// others serve HTTP.
var networkImports = map[string][]string{
	livehttpPkg: {"net", "net/http"},
	hubPkg:      {"net", "net/http"},
	perfpagePkg: {"net/http"},
	mcphttpPkg:  {"net/http"},
}

// clientSymbols are functions every outgoing connection or name lookup
// through the standard library runs. crypto/tls's client handshake is not
// one of them: net/http's server links it too, since it wraps an accepted
// unencrypted HTTP/2 connection with tls.Client, so nm cannot tell it
// apart. Nor is (*net.Resolver).LookupIPAddr: the compiler always inlines
// it into its callers, so it never has a symbol, and the lookupIPAddr it
// calls is linked by the listener's address parsing too. The source scan
// refuses calls of it.
var clientSymbols = []string{
	"net/http.(*Client).do",
	"net/http.(*Transport).roundTrip",
	"net/http.(*Transport).dialConn",
	"net.Dial",
	"net.DialTimeout",
	"net.(*Resolver).LookupHost",
}

// liveSymbols are functions of the hub's handlers: the page's routes, its
// event stream and the MCP transport. The linker keeps only code a request
// can reach, so the client symbols' absence says something about the
// handlers only while these are linked. Each is reached only through the
// http.Handler interface, so the compiler cannot inline it away: an
// inlined function could be missing from the symbol table and still be
// reachable.
var liveSymbols = []string{
	perfpagePkg + ".(*routes).ServeHTTP",
	livehttpPkg + ".(*Hub).ServeHTTP",
	mcphttpPkg + ".(*Transport).ServeHTTP",
}

// hubImports are the hub's non-standard packages on target, one of
// binarytest.Targets, each with its imports.
func hubImports(t *testing.T, target string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", `{{if not .Standard}}{{.ImportPath}}: {{join .Imports " "}}{{end}}`, ".")
	cmd.Env = append(os.Environ(), binarytest.TargetEnv(target)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list for %s: %v", target, err)
	}
	pkgs := map[string][]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		path, imports, _ := strings.Cut(line, ": ")
		pkgs[path] = strings.Fields(imports)
	}
	if _, ok := pkgs[hubPkg]; !ok {
		t.Fatalf("go list for %s lists %d packages without %s: not the hub's dependencies", target, len(pkgs), hubPkg)
	}
	return pkgs
}

// TestTheHubBinaryOpensNoConnection is a layer under the source scan
// (TestNoClientNetworkingInTheServer) and the socket test
// (TestEverySocketIsLoopback): on every target, only the packages of
// networkImports import a networking package, each only those it names,
// and the binary built for this host
// has none of the client symbols while it links the hub's handlers. It
// cannot replace either: a call of (*net.Dialer).DialContext, which
// tls.Dial makes too, is linked by the server side as well, so nm cannot
// tell it apart. It replaces the stdio server's
// TestTheServerOpensNoConnection, from before the network moved here.
func TestTheHubBinaryOpensNoConnection(t *testing.T) {
	for _, target := range binarytest.Targets {
		for path, imports := range hubImports(t, target) {
			for _, imp := range imports {
				networking := slices.Contains(networkingPackages, imp) || strings.HasPrefix(imp, "net/http/")
				if networking && !slices.Contains(networkImports[path], imp) {
					t.Errorf("%s imports %s on %s; only %v may", path, imp, target, networkImports)
				}
			}
		}
	}

	bin, err := hubBinary()
	if err != nil {
		t.Fatal(err)
	}
	syms, err := exec.Command("go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatalf("go tool nm: %v", err)
	}
	linked := map[string]bool{}
	for line := range strings.SplitSeq(string(syms), "\n") {
		if f := strings.Fields(line); len(f) >= 3 {
			linked[f[len(f)-1]] = true
		}
	}
	for _, sym := range liveSymbols {
		if !linked[sym] {
			t.Errorf("the binary does not link %s: the hub's handlers are unreachable, so finding no client symbol proves nothing about them", sym)
		}
	}
	for _, sym := range clientSymbols {
		if linked[sym] {
			t.Errorf("the binary links %s, which opens a connection or looks a name up", sym)
		}
	}
}

// The positive half of perf-agent-mcp's TestTheServerLinksNoNetworking:
// the hub links the page's routes, the listener and the MCP transport,
// so the stdio binary's not linking them is not a test that cannot fail.
func TestTheHubBinaryLinksThePageAndTransport(t *testing.T) {
	for _, target := range binarytest.Targets {
		pkgs := hubImports(t, target)
		for _, want := range []string{perfpagePkg, livehttpPkg, mcphttpPkg} {
			if _, ok := pkgs[want]; !ok {
				t.Errorf("the hub does not link %s on %s", want, target)
			}
		}
	}
}
