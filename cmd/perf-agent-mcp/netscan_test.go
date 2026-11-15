package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// clientNames are the package-level names, by import path, that dial,
// resolve a name, make an HTTP request or open a socket other than a TCP
// listener. No package of the server may use one.
var clientNames = map[string][]string{
	"net": {
		"Dial", "DialTCP", "DialUDP", "DialIP", "DialUnix", "DialTimeout", "Dialer",
		"Resolver", "DefaultResolver",
		"LookupHost", "LookupIP", "LookupAddr", "LookupCNAME", "LookupTXT", "LookupMX", "LookupSRV", "LookupNS", "LookupPort",
		"ListenPacket", "ListenUDP", "ListenIP", "ListenUnixgram", "ListenMulticastUDP", "FileConn", "FilePacketConn",
	},
	"net/http": {
		"Get", "Post", "PostForm", "Head", "Client", "DefaultClient", "DefaultTransport", "Transport", "RoundTripper",
		"NewRequest", "NewRequestWithContext", "ProxyFromEnvironment", "ProxyURL",
	},
	"crypto/tls":            {"Dial", "DialWithDialer", "Dialer", "Client"},
	"syscall":               {"Socket", "Connect", "Sendto", "Sendmsg"},
	"golang.org/x/sys/unix": {"Socket", "Connect", "Sendto", "Sendmsg"},
}

// listenNames open a listener. Only internal/livehttp/listen.go, behind
// its loopback check, may use one.
var listenNames = map[string][]string{
	"net":      {"Listen", "ListenTCP", "ListenUnix", "ListenConfig", "FileListener"},
	"net/http": {"ListenAndServe", "ListenAndServeTLS"},
}

// listenFile is the one file allowed a listening call.
const listenFile = "internal/livehttp/listen.go"

// clientMethods are method names that dial, resolve or listen whatever
// their receiver, so a dialer or client reached without naming its type
// is caught too.
var clientMethods = []string{
	"DialContext", "DialTLSContext", "RoundTrip", "LookupIPAddr", "LookupNetIP",
	"ListenAndServe", "ListenAndServeTLS", "ServeTLS",
}

// clientPackages are standard packages that exist to make requests; no
// package of the server may import one.
var clientPackages = []string{
	"net/smtp", "net/rpc", "net/rpc/jsonrpc", "net/http/httputil", "net/http/httptest",
	"net/http/httptrace", "net/http/cookiejar", "net/http/cgi", "net/http/fcgi",
}

// scanned is what scanNetworking found in a set of files.
type scanned struct {
	findings []string
	// listens are the listening calls in the allowed file, as
	// "pkg.Name".
	listens []string
}

// scanNetworking parses every file and reports each use of a client name,
// a client method, a client package, or a listening call outside
// allowListen. A dot import of a networking package is a finding too, as
// its names could not be told apart. Import names are resolved per file.
func scanNetworking(t *testing.T, files []string, display func(string) string, allowListen string) scanned {
	t.Helper()
	var out scanned
	fset := token.NewFileSet()
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		name := display(file)
		report := func(n ast.Node, format string, args ...any) {
			p := fset.Position(n.Pos())
			out.findings = append(out.findings, fmt.Sprintf("%s:%d: %s", name, p.Line, fmt.Sprintf(format, args...)))
		}
		local := map[string]string{} // import name in this file -> path
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", name, imp.Path.Value, err)
			}
			if slices.Contains(clientPackages, p) {
				report(imp, "imports %s", p)
			}
			_, client := clientNames[p]
			_, listen := listenNames[p]
			n := path.Base(p)
			if imp.Name != nil {
				n = imp.Name.Name
			}
			switch n {
			case "_":
			case ".":
				if client || listen {
					report(imp, "dot-imports %s, so its names cannot be checked", p)
				}
			default:
				local[n] = p
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok {
				if p, ok := local[x.Name]; ok {
					qual := p + "." + sel.Sel.Name
					switch {
					case slices.Contains(clientNames[p], sel.Sel.Name):
						report(sel, "uses %s", qual)
					case slices.Contains(listenNames[p], sel.Sel.Name):
						if name == allowListen {
							out.listens = append(out.listens, qual)
						} else {
							report(sel, "uses %s outside %s", qual, allowListen)
						}
					}
					return true
				}
			}
			if slices.Contains(clientMethods, sel.Sel.Name) {
				report(sel, "calls a method named %s", sel.Sel.Name)
			}
			return true
		})
	}
	return out
}

// TestNoClientNetworkingInTheServer reads the source of every non-standard
// package the binary links, internal/livehttp's included, and finds no
// dial, name lookup, HTTP client or non-TCP socket, and no listening call
// but the one in internal/livehttp/listen.go, which checks the address is
// loopback first.
func TestNoClientNetworkingInTheServer(t *testing.T) {
	out, err := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}} {{.Dir}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var files, pkgs []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg, dir, ok := strings.Cut(line, " ")
		if !ok || dir == "" {
			t.Fatalf("go list line %q has no directory", line)
		}
		pkgs = append(pkgs, pkg)
		// Every non-test file, whatever its build constraints.
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				files = append(files, m)
			}
		}
	}
	for _, want := range []string{
		"github.com/dpsoft/perf-agent/internal/livehttp",
		"github.com/dpsoft/perf-agent/internal/perfmcp",
		"github.com/dpsoft/perf-agent/internal/mcpstdio",
		"github.com/google/pprof/profile",
	} {
		if !slices.Contains(pkgs, want) {
			t.Errorf("the scan does not cover %s; it covers %v", want, pkgs)
		}
	}
	display := func(file string) string {
		if rel, err := filepath.Rel(root, file); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		return file
	}
	got := scanNetworking(t, files, display, listenFile)
	for _, f := range got.findings {
		t.Error(f)
	}
	// The one listening call is found, so the scan reads the file that
	// holds it and resolves its import.
	if !slices.Equal(got.listens, []string{"net.Listen"}) {
		t.Errorf("listening calls in %s = %v, want exactly net.Listen", listenFile, got.listens)
	}
	t.Logf("scanned %d files in %d packages", len(files), len(pkgs))
}

// The scan resolves import names per file, sees through renamed imports,
// refuses dot imports and catches client methods on any receiver.
func TestTheNetworkScanSeesRenamedImports(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	files := []string{
		write("a.go", "package x\nimport h \"net/http\"\nfunc a() { _, _ = h.Get(\"\") }\n"),
		write("b.go", "package x\nimport secure \"crypto/tls\"\nvar _ = secure.Dial\n"),
		write("c.go", "package x\nimport . \"net\"\nvar _ = 1\n"),
		write("d.go", "package x\nimport \"context\"\ntype d interface{ DialContext(context.Context, string, string) (any, error) }\nfunc f(x d) { _, _ = x.DialContext(nil, \"\", \"\") }\n"),
		write("e.go", "package x\nimport n \"net\"\nfunc e() { _, _ = n.Listen(\"tcp\", \"\") }\n"),
		write("listen.go", "package x\nimport \"net\"\nfunc l() { _, _ = net.Listen(\"tcp\", \"\") }\n"),
		// http here is not net/http: no import of it, so nothing to report.
		write("f.go", "package x\ntype t struct{ Get int }\nvar http t\nvar _ = http.Get\n"),
	}
	got := scanNetworking(t, files, filepath.Base, "listen.go")
	want := []string{
		"a.go:3: uses net/http.Get",
		"b.go:3: uses crypto/tls.Dial",
		"c.go:2: dot-imports net, so its names cannot be checked",
		"d.go:4: calls a method named DialContext",
		"e.go:3: uses net.Listen outside listen.go",
	}
	if !slices.Equal(got.findings, want) {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got.findings, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Equal(got.listens, []string{"net.Listen"}) {
		t.Errorf("listens = %v", got.listens)
	}
}

// procSocket is one row of /proc/<pid>/net/{tcp,tcp6,udp,udp6}.
type procSocket struct {
	table         string
	local, remote netip.AddrPort
	listening     bool
}

// procSockets reads pid's socket tables, keyed by inode. The tables list
// every socket of the network namespace; the caller picks the process's own
// by the inodes of its descriptors.
func procSockets(t *testing.T, pid int) map[string]procSocket {
	t.Helper()
	out := map[string]procSocket{}
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, table))
		if err != nil {
			if os.IsNotExist(err) {
				continue // no IPv6 in this kernel
			}
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		for _, line := range lines[1:] {
			f := strings.Fields(line)
			if len(f) < 10 {
				t.Fatalf("%s: short line %q", table, line)
			}
			out[f[9]] = procSocket{
				table: table, local: procAddr(t, f[1]), remote: procAddr(t, f[2]),
				// 0A is TCP_LISTEN.
				listening: strings.HasPrefix(table, "tcp") && f[3] == "0A",
			}
		}
	}
	return out
}

// procAddr parses an address as the kernel prints it: the IP as 32-bit
// words in host byte order, each in hex, then the port in hex.
func procAddr(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ipHex, portHex, ok := strings.Cut(s, ":")
	port, err := strconv.ParseUint(portHex, 16, 16)
	if !ok || err != nil || (len(ipHex) != 8 && len(ipHex) != 32) {
		t.Fatalf("socket address %q", s)
	}
	raw := make([]byte, 0, 16)
	for i := 0; i < len(ipHex); i += 8 {
		w, err := strconv.ParseUint(ipHex[i:i+8], 16, 32)
		if err != nil {
			t.Fatalf("socket address %q: %v", s, err)
		}
		raw = binary.NativeEndian.AppendUint32(raw, uint32(w))
	}
	ip, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(ip.Unmap(), uint16(port))
}

// socketInodes are the inodes of pid's socket descriptors.
func socketInodes(t *testing.T, pid int) []string {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		link, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // closed since ReadDir
		}
		if inode, ok := strings.CutPrefix(link, "socket:["); ok {
			out = append(out, strings.TrimSuffix(inode, "]"))
		}
	}
	return out
}

// TestEverySocketIsLoopback runs the built server with the live page, has
// a browser read it, and checks every socket the process holds: each is
// TCP on a loopback address, listening or connected to a loopback peer.
// When its input ends it exits 0 and the port is closed.
func TestEverySocketIsLoopback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the process's sockets from /proc/<pid>/fd and /proc/<pid>/net, which only Linux has")
	}
	bin := buildServer(t)
	profiles, output := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(profiles, "cpu.pb.gz"), pmfixture.CPUBasic(), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--profiles", profiles, "--output", output, "--live")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// exited is closed once the process is reaped, with its status in
	// waitErr; the cleanup kills a server that outlives the test.
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	c := &stdioClient{t: t, w: stdin, r: bufio.NewReaderSize(stdout, 1<<20), nextID: 1}
	c.call("initialize", map[string]any{"protocolVersion": "2025-11-25"})
	pageURL, pub := c.openAndPublish()

	// The test is the browser: a stream, a model and the shell.
	browser := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	stream := browserGet(t, browser, pageURL+"events")
	defer func() { _ = stream.Body.Close() }()
	if line, err := bufio.NewReader(stream.Body).ReadString('\n'); err != nil || line != "event: state\n" {
		t.Fatalf("stream: %q (%v)", line, err)
	}
	for _, route := range []string{"pub/" + pub + "/model", ""} {
		res := browserGet(t, browser, pageURL+route)
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}

	sockets := procSockets(t, cmd.Process.Pid)
	inodes := socketInodes(t, cmd.Process.Pid)
	var listening, connected int
	for _, inode := range inodes {
		s, ok := sockets[inode]
		switch {
		case !ok:
			t.Errorf("socket %s is not in the TCP or UDP tables: not a TCP socket", inode)
			continue
		case strings.HasPrefix(s.table, "udp"):
			t.Errorf("socket %s is UDP (%s, local %v)", inode, s.table, s.local)
		case !s.local.Addr().IsLoopback():
			t.Errorf("socket %s is bound to %v, not loopback", inode, s.local)
		case s.listening:
			listening++
		case !s.remote.Addr().IsLoopback():
			t.Errorf("socket %s (local %v) is connected to %v, not loopback", inode, s.local, s.remote)
		default:
			connected++
		}
	}
	// The browser's connections are open, so the check saw the listener
	// and at least the stream's connection.
	if listening != 1 || connected < 1 {
		t.Errorf("%d listening and %d connected sockets among %d, want 1 and at least 1", listening, connected, len(inodes))
	}

	_ = stdin.Close()
	select {
	case <-exited:
		if waitErr != nil {
			t.Errorf("exit: %v; stderr:\n%s", waitErr, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server is still running 5s after its input ended")
	}
	addr := strings.TrimPrefix(strings.SplitN(pageURL, "/s/", 2)[0], "http://")
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("%s still accepts connections after the server exited", addr)
	}
}

// browserGet is a same-origin GET of url that must answer 200.
func browserGet(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", redact(url), err)
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		t.Fatalf("GET %s: %s", redact(url), res.Status)
	}
	return res
}

// redact drops the token from a page URL for a failure message.
func redact(url string) string {
	before, after, ok := strings.Cut(url, "/s/")
	if !ok {
		return url
	}
	_, rest, _ := strings.Cut(after, "/")
	return before + "/s/<token>/" + rest
}

// openAndPublish opens cpu.pb.gz, starts the page and publishes the
// default view's facts, returning the page's URL and the publication.
func (c *stdioClient) openAndPublish() (pageURL, pub string) {
	c.t.Helper()
	var o struct {
		Handle      string `json:"handle"`
		DefaultView string `json:"default_view"`
	}
	if err := json.Unmarshal([]byte(c.tool("open", map[string]any{"path": "cpu.pb.gz"})), &o); err != nil {
		c.t.Fatal(err)
	}
	var opened struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(c.tool("page_open", map[string]any{})), &opened); err != nil || opened.URL == "" {
		c.t.Fatalf("page_open: %+v (%v)", opened, err)
	}
	var published struct {
		Status      string `json:"status"`
		Publication string `json:"publication"`
		URL         string `json:"url"`
	}
	text := c.tool("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}})
	if err := json.Unmarshal([]byte(text), &published); err != nil || published.Status != "published" || published.URL != opened.URL {
		c.t.Fatalf("page_explain: %s (%v)", text, err)
	}
	return opened.URL, published.Publication
}
