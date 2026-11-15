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
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// rawCalls are the syscall and x/sys/unix names that open or use a socket
// or start a process behind the net and os/exec packages' backs. The
// server's own system call uses are O_NONBLOCK and the SIGINT and SIGTERM
// constants.
var rawCalls = []string{
	"Socket", "Connect", "Bind", "Listen", "Sendto", "Sendmsg", "SendmsgN", "Sendmmsg",
	"Syscall", "Syscall6", "RawSyscall", "RawSyscall6",
	"ForkExec", "StartProcess", "Exec",
}

// clientNames are the package-level names, by import path, that dial,
// resolve a name, make an HTTP request, open a socket other than a TCP
// listener, make a raw system call or start a process. No package of the
// server may use one.
var clientNames = map[string][]string{
	"net": {
		"Dial", "DialTCP", "DialUDP", "DialIP", "DialUnix", "DialTimeout", "Dialer",
		"Resolver", "DefaultResolver",
		"LookupHost", "LookupIP", "LookupAddr", "LookupCNAME", "LookupTXT", "LookupMX", "LookupSRV", "LookupNS", "LookupPort",
		// They resolve a hostname through DefaultResolver.
		"ResolveTCPAddr", "ResolveUDPAddr", "ResolveIPAddr",
		"ListenPacket", "ListenUDP", "ListenIP", "ListenUnixgram", "ListenMulticastUDP", "FileConn", "FilePacketConn",
	},
	"net/http": {
		"Get", "Post", "PostForm", "Head", "Client", "DefaultClient", "DefaultTransport", "Transport", "RoundTripper",
		"NewRequest", "NewRequestWithContext", "ProxyFromEnvironment", "ProxyURL",
	},
	"crypto/tls":            {"Dial", "DialWithDialer", "Dialer", "Client"},
	"syscall":               rawCalls,
	"golang.org/x/sys/unix": rawCalls,
	"os":                    {"StartProcess"},
}

// listenNames open a listener. Only internal/livehttp/listen.go, behind
// its loopback check, may use one.
var listenNames = map[string][]string{
	"net":      {"Listen", "ListenTCP", "ListenUnix", "ListenConfig", "FileListener"},
	"net/http": {"ListenAndServe", "ListenAndServeTLS"},
}

// listenFile is the one file allowed a listening call; execFile is the
// one file allowed to import os/exec, for the offline git probe.
const (
	listenFile = "internal/livehttp/listen.go"
	execFile   = "internal/sourceroot/git.go"
)

// allowedFiles names, as the scan displays them, the files allowed a
// listening call and an os/exec import.
type allowedFiles struct{ listen, exec string }

// clientMethods are names that dial, resolve or listen whatever their
// receiver, so a dialer or client reached without naming its type is
// caught too, and so is a package function of the same name.
var clientMethods = []string{
	"DialContext", "DialTLSContext", "RoundTrip", "LookupIPAddr", "LookupNetIP",
	"ListenAndServe", "ListenAndServeTLS", "ServeTLS",
}

// clientPackages are standard packages that exist to make requests; no
// package of the server may import one.
var clientPackages = []string{
	"net/smtp", "net/rpc", "net/rpc/jsonrpc", "net/http/httputil", "net/http/httptest",
	"net/http/httptrace", "net/http/cookiejar", "net/http/cgi", "net/http/fcgi",
	"net/textproto", "log/syslog",
}

// scanned is what scanNetworking found in a set of files.
type scanned struct {
	findings []string
	// listens are the listening calls in the allowed file, as
	// "pkg.Name"; exec says the allowed file imports os/exec.
	listens []string
	exec    bool
}

// scanNetworking parses every file and reports each use of a client name
// or client method, an import of a client package, a listening call
// outside allow.listen and an os/exec import outside allow.exec. A dot
// import of a networking package is a finding too, as its names could not
// be told apart, and so are cgo and go:linkname, which reach code the scan
// cannot read. Import names are resolved per file.
func scanNetworking(t *testing.T, files []string, display func(string) string, allow allowedFiles) scanned {
	t.Helper()
	var out scanned
	fset := token.NewFileSet()
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution|parser.ParseComments)
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
			switch {
			case slices.Contains(clientPackages, p):
				report(imp, "imports %s", p)
			case p == "C":
				report(imp, "imports C: cgo the scan cannot read")
			case p == "os/exec" && name == allow.exec:
				out.exec = true
			case p == "os/exec":
				report(imp, "imports os/exec outside %s", allow.exec)
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
		for _, g := range f.Comments {
			for _, c := range g.List {
				if strings.HasPrefix(c.Text, "//go:linkname") {
					report(c, "has a go:linkname directive")
				}
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
					case slices.Contains(listenNames[p], sel.Sel.Name):
						if name == allow.listen {
							out.listens = append(out.listens, qual)
						} else {
							report(sel, "uses %s outside %s", qual, allow.listen)
						}
					case slices.Contains(clientNames[p], sel.Sel.Name), slices.Contains(clientMethods, sel.Sel.Name):
						report(sel, "uses %s", qual)
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

// listedPackage is one non-standard package of go list -deps, with the
// number of its files that are not Go: cgo, C, C++, Objective-C, Fortran,
// assembly and system objects.
type listedPackage struct {
	path, dir string
	native    []int
}

// nativeKinds names listedPackage.native's counts, in order.
var nativeKinds = []string{"cgo", "C", "C++", "Objective-C", "Fortran", "assembly", "syso"}

// listPackages runs go list -deps on pattern in dir with cgo enabled, so
// cgo files are counted rather than ignored, and returns its non-standard
// packages.
func listPackages(t *testing.T, dir, pattern string, env ...string) []listedPackage {
	t.Helper()
	cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f",
		"{{if not .Standard}}{{.ImportPath}}\t{{.Dir}}\t{{len .CgoFiles}} {{len .CFiles}} {{len .CXXFiles}} {{len .MFiles}} {{len .FFiles}} {{len .SFiles}} {{len .SysoFiles}}{{end}}",
		pattern)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "CGO_ENABLED=1"), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var pkgs []listedPackage
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[1] == "" {
			t.Fatalf("go list line %q", line)
		}
		p := listedPackage{path: f[0], dir: f[1]}
		for c := range strings.FieldsSeq(f[2]) {
			n, err := strconv.Atoi(c)
			if err != nil {
				t.Fatalf("go list line %q: %v", line, err)
			}
			p.native = append(p.native, n)
		}
		if len(p.native) != len(nativeKinds) {
			t.Fatalf("go list line %q", line)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// nativeCode says which files of p are not Go, or "" when all are: code
// the source scan cannot read.
func nativeCode(p listedPackage) string {
	var kinds []string
	for i, n := range p.native {
		if n > 0 {
			kinds = append(kinds, fmt.Sprintf("%d %s", n, nativeKinds[i]))
		}
	}
	return strings.Join(kinds, ", ")
}

// TestNoClientNetworkingInTheServer reads the source of every non-standard
// package the binary links, internal/livehttp's included, and finds no
// dial, name lookup, HTTP client, non-TCP socket or raw system call, no
// listening call but the one in internal/livehttp/listen.go, which checks
// the address is loopback first, and no os/exec outside the git probe.
// None of those packages has code the scan cannot read: no cgo, assembly
// or object files, and no go:linkname.
func TestNoClientNetworkingInTheServer(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var files, pkgs []string
	for _, p := range listPackages(t, ".", ".") {
		pkgs = append(pkgs, p.path)
		if native := nativeCode(p); native != "" {
			t.Errorf("%s has files the scan cannot read: %s", p.path, native)
		}
		// Every non-test file, whatever its build constraints.
		matches, err := filepath.Glob(filepath.Join(p.dir, "*.go"))
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
		"github.com/dpsoft/perf-agent/internal/sourceroot",
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
	got := scanNetworking(t, files, display, allowedFiles{listen: listenFile, exec: execFile})
	for _, f := range got.findings {
		t.Error(f)
	}
	// The one listening call and the one os/exec import are found, so the
	// scan reads the files that hold them and resolves their imports.
	if !slices.Equal(got.listens, []string{"net.Listen"}) {
		t.Errorf("listening calls in %s = %v, want exactly net.Listen", listenFile, got.listens)
	}
	if !got.exec {
		t.Errorf("the scan did not see %s import os/exec", execFile)
	}
	t.Logf("scanned %d files in %d packages", len(files), len(pkgs))
}

// writeFixtures writes files, named by their keys, into a new directory
// and returns their paths, sorted.
func writeFixtures(t *testing.T, files map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for name, src := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths
}

// The scan resolves import names per file, sees through renamed imports,
// refuses dot imports and catches client methods on any receiver.
func TestTheNetworkScanSeesRenamedImports(t *testing.T) {
	files := writeFixtures(t, map[string]string{
		"a.go": "package x\nimport h \"net/http\"\nfunc a() { _, _ = h.Get(\"\") }\n",
		"b.go": "package x\nimport secure \"crypto/tls\"\nvar _ = secure.Dial\n",
		"c.go": "package x\nimport . \"net\"\nvar _ = 1\n",
		"d.go": "package x\nimport \"context\"\ntype d interface{ DialContext(context.Context, string, string) (any, error) }\nfunc f(x d) { _, _ = x.DialContext(nil, \"\", \"\") }\n",
		"e.go": "package x\nimport n \"net\"\nfunc e() { _, _ = n.Listen(\"tcp\", \"\") }\n",
		// http here is not net/http: no import of it, so nothing to report.
		"f.go":      "package x\ntype t struct{ Get int }\nvar http t\nvar _ = http.Get\n",
		"listen.go": "package x\nimport \"net\"\nfunc l() { _, _ = net.Listen(\"tcp\", \"\") }\n",
	})
	got := scanNetworking(t, files, filepath.Base, allowedFiles{listen: "listen.go", exec: "git.go"})
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

// The scan finds raw system calls and process starts, name resolution
// through the Resolve functions, client functions called through their
// package, os/exec outside the allowed file, client packages, go:linkname
// and cgo, each through a renamed import where it has one.
func TestTheNetworkScanSeesSyscallsExecAndDirectives(t *testing.T) {
	files := writeFixtures(t, map[string]string{
		"g.go": "package x\nimport sc \"syscall\"\nvar _ = sc.Bind\nvar _ = sc.Listen\nfunc g() { _, _, _ = sc.RawSyscall(0, 0, 0, 0); _, _, _ = sc.Syscall6(0, 0, 0, 0, 0, 0, 0) }\nvar _ = sc.ForkExec\n",
		"h.go": "package x\nimport u \"golang.org/x/sys/unix\"\nvar _ = u.Sendmmsg\nvar _ = u.SendmsgN\n",
		"i.go": "package x\nimport nn \"net\"\nvar _, _ = nn.ResolveTCPAddr(\"tcp\", \"\")\nvar _, _ = nn.ResolveUDPAddr(\"udp\", \"\")\nvar _, _ = nn.ResolveIPAddr(\"ip\", \"\")\n",
		"j.go": "package x\nimport web \"net/http\"\nvar _ = web.ServeTLS\n",
		"k.go": "package x\nimport \"os/exec\"\nvar _ = exec.Command\n",
		"l.go": "package x\nimport (\n\t_ \"log/syslog\"\n\ttp \"net/textproto\"\n)\nvar _ = tp.Dial\n",
		"m.go": "package x\nimport _ \"unsafe\"\n\n//go:linkname now runtime.nanotime\nfunc now() int64\n",
		"n.go": "package x\n\n// int one(void) { return 1; }\nimport \"C\"\n",
		"o.go": "package x\nimport \"os\"\nvar _ = os.StartProcess\n",
		// The allowed file may import os/exec, renamed or not.
		"git.go": "package x\nimport run \"os/exec\"\nvar _ = run.Command\n",
	})
	got := scanNetworking(t, files, filepath.Base, allowedFiles{listen: "listen.go", exec: "git.go"})
	want := []string{
		"g.go:3: uses syscall.Bind",
		"g.go:4: uses syscall.Listen",
		"g.go:5: uses syscall.RawSyscall",
		"g.go:5: uses syscall.Syscall6",
		"g.go:6: uses syscall.ForkExec",
		"h.go:3: uses golang.org/x/sys/unix.Sendmmsg",
		"h.go:4: uses golang.org/x/sys/unix.SendmsgN",
		"i.go:3: uses net.ResolveTCPAddr",
		"i.go:4: uses net.ResolveUDPAddr",
		"i.go:5: uses net.ResolveIPAddr",
		"j.go:3: uses net/http.ServeTLS",
		"k.go:2: imports os/exec outside git.go",
		"l.go:3: imports log/syslog",
		"l.go:4: imports net/textproto",
		"m.go:4: has a go:linkname directive",
		"n.go:4: imports C: cgo the scan cannot read",
		"o.go:3: uses os.StartProcess",
	}
	if !slices.Equal(got.findings, want) {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got.findings, "\n"), strings.Join(want, "\n"))
	}
	if !got.exec {
		t.Error("the allowed os/exec import was not seen")
	}
}

// listPackages counts every kind of file the scan cannot read.
func TestListPackagesCountsNativeFiles(t *testing.T) {
	dir := t.TempDir()
	for name, src := range map[string]string{
		"go.mod": "module example.com/native\n\ngo 1.26\n",
		"c.go":   "package native\n\n// int one(void);\nimport \"C\"\n",
		"one.c":  "int one(void) { return 1; }\n",
		"two.cc": "int two() { return 2; }\n",
		"t.S":    "\n",
		"x.syso": "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkgs := listPackages(t, dir, ".", "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
	if len(pkgs) != 1 {
		t.Fatalf("listed %+v", pkgs)
	}
	if got, want := nativeCode(pkgs[0]), "1 cgo, 1 C, 1 C++, 1 assembly, 1 syso"; got != want {
		t.Errorf("native code %q, want %q", got, want)
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
	c := &stdioClient{t: t, w: stdin, r: bufio.NewReaderSize(stdout, 1<<20), nextID: 1}
	// finish ends the server's input, reads its stdout to the end, and only
	// then reaps it, as exec requires of a StdoutPipe. A server still
	// running 5s later is killed, which its status then shows. The test
	// calls it, and the cleanup calls it again for a test that failed
	// first.
	finish := sync.OnceValues(func() (rest []byte, waitErr error) {
		_ = stdin.Close()
		read := make(chan []byte, 1)
		go func() {
			b, _ := io.ReadAll(c.r)
			read <- b
		}()
		select {
		case rest = <-read:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			rest = <-read
		}
		return rest, cmd.Wait()
	})
	t.Cleanup(func() { _, _ = finish() })

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

	pid := cmd.Process.Pid
	sockets := procSockets(t, pid)
	inodes := socketInodes(t, pid)
	// A socket made after the tables were read is missing from them: read
	// them once more before calling it a socket of another kind, and skip
	// one the process no longer holds.
	if slices.ContainsFunc(inodes, func(i string) bool { _, ok := sockets[i]; return !ok }) {
		sockets = procSockets(t, pid)
		held := socketInodes(t, pid)
		inodes = slices.DeleteFunc(inodes, func(i string) bool {
			_, ok := sockets[i]
			return !ok && !slices.Contains(held, i)
		})
	}
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

	rest, err := finish()
	if err != nil {
		t.Errorf("exit after the input ended: %v; stderr:\n%s", err, stderr.String())
	}
	if len(rest) != 0 {
		t.Errorf("stdout carried %q after the last response", rest)
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
