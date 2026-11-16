// The source scan in this file is drift protection for code review. It
// fails the build when a change adds a network client, a listener other
// than the reviewed one, a raw system call or a process start outside the
// few reviewed call sites, so such an addition is seen before it lands.
// It is not a defence against a committer set on evading it. It resolves
// names with go/types and holds the reviewed files, and the calls of their
// process-start helpers, to the exact shapes below, but it follows no
// values beyond those rules: what the helpers' callers are passed (the
// reaper's spec, startReaperProcess's and startLauncher's callers), what
// a function returns, reflect and unsafe are not tracked. String-building
// evasions (concatenated literals, bytes, a path read at run time) are
// out of its scope: the shell rule matches whole literals only. It reads
// every non-test Go file of the packages linked for linux/amd64,
// linux/arm64 and darwin/arm64, a file for another target or a custom tag
// included: those are held to the name rules only, as only the three
// targets' files are type-checked, and an import of a non-standard
// package none of the targets links is a finding.

package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
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
)

// rawCalls are the syscall and x/sys/unix names that open or use a socket,
// start a process behind the net and os/exec packages' backs, or change
// the process's credentials, namespaces or root. The server's own system call
// uses are O_NONBLOCK and the SIGINT and SIGTERM constants.
var rawCalls = []string{
	"Socket", "Connect", "Bind", "Listen", "Sendto", "Sendmsg", "SendmsgN", "Sendmmsg",
	"Syscall", "Syscall6", "RawSyscall", "RawSyscall6",
	"AllThreadsSyscall", "AllThreadsSyscall6", "SyscallNoError", "RawSyscallNoError",
	"ForkExec", "StartProcess", "Exec",
	"Mount", "Unshare", "Chroot", "PivotRoot",
	"Setuid", "Setgid", "Setresuid", "Setresgid", "Setreuid", "Setregid", "Setgroups", "Setfsuid", "Setfsgid",
}

// rawPackages are the packages rawCalls are denied in, and rawPrefixes
// the name prefixes denied there too: the ptrace wrappers, which write to
// another process.
var (
	rawPackages = []string{"syscall", "golang.org/x/sys/unix"}
	rawPrefixes = []string{"Ptrace"}
)

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

// clientName reports whether name, in the package with import path p, is
// one the server may not use.
func clientName(p, name string) bool {
	return slices.Contains(clientNames[p], name) ||
		slices.Contains(rawPackages, p) && slices.ContainsFunc(rawPrefixes, func(pre string) bool { return strings.HasPrefix(name, pre) })
}

// listenNames open a listener. Only internal/livehttp/listen.go, behind
// its loopback check, may use one.
var listenNames = map[string][]string{
	"net":      {"Listen", "ListenTCP", "ListenUnix", "ListenConfig", "FileListener"},
	"net/http": {"ListenAndServe", "ListenAndServeTLS"},
}

// listenFile is the one file allowed a listening call. commandFile and
// gitFile are the files allowed to import os/exec, each held to its
// execRules: capture's process starts and the offline git probe.
// launchFile is the one file allowed syscall.Exec and, for two prctl
// options, syscall.RawSyscall. No file under shellFreeDir names a shell,
// sudo or pkexec in a string.
const (
	listenFile   = "internal/livehttp/listen.go"
	commandFile  = "internal/capture/exec_linux.go"
	gitFile      = "internal/sourceroot/git.go"
	launchFile   = "internal/capture/launch_linux.go"
	attachFile   = "internal/capture/internal_linux.go"
	shellFreeDir = "internal/capture/"
)

// allowedFiles names, as the scan displays them, the file allowed a
// listening call, the files allowed os/exec with what each may use of it,
// the file allowed the launcher's system calls, and the prefix of the
// files that must name no shell ("" for every file).
type allowedFiles struct {
	listen  string
	exec    map[string]execRule
	launch  string
	noShell string
	// helpers are the process-start helpers, by name, with their
	// reviewed callers.
	helpers map[string]helperRule
	// covered are the import paths of the non-standard packages the scan
	// reads; an import of another one is a finding. nil checks none.
	covered map[string]bool
}

// helperRule is a process-start helper declared in file, and the only
// calls of it allowed.
type helperRule struct {
	file  string
	calls []helperCall
}

// helperCall is a reviewed call of a helper: once, in the function fn of
// file, its first argument written exactly as arg0, whose variable (arg0's
// root, for a field) is fn's receiver or parameter, never reassigned.
type helperCall struct {
	file, fn, arg0 string
}

// execRule is what a file allowed os/exec may use of it: the calls, each
// once, and the types it may name. Anything else of os/exec there is a
// finding.
type execRule struct {
	calls []allowedCall
	types []string
}

// allowedCall is one os/exec call a file may make: in the function fn,
// written exactly as form with the package named exec. Every variable in
// its arguments is declared in fn's own scope and never assigned again or
// has its address taken; those named in params are fn's parameters, and
// each named in defs is defined, alone or first, by the allowed call of fn
// written as its value.
type allowedCall struct {
	fn, form string
	params   []string
	defs     map[string]string
}

// execRules are the reviewed process starts. The server's own executable
// runs in its reaper and launcher modes and the verified perf-agent runs,
// each with argv and environment set on the Cmd; git is found on PATH and
// run with the probe's options.
var execRules = map[string]execRule{
	commandFile: {
		calls: []allowedCall{
			{fn: "selfCommand", form: "exec.Command(self)", params: []string{"self"}},
			{fn: "startPerfAgent", form: "exec.Command(path)", params: []string{"path"}},
		},
		types: []string{"Cmd"},
	},
	gitFile: {
		calls: []allowedCall{
			{fn: "revParse", form: `exec.LookPath("git")`},
			{fn: "revParse", form: "exec.CommandContext(ctx, bin, argv...)", defs: map[string]string{"bin": `exec.LookPath("git")`}},
		},
		types: []string{"ExitError"},
	},
}

// helperRules are the helpers of commandFile that start a process: the
// server's own executable from the reaper and launcher starts, and the
// verified perf-agent from the reaper's attach.
var helperRules = map[string]helperRule{
	"selfCommand": {file: commandFile, calls: []helperCall{
		{file: commandFile, fn: "startReaperProcess", arg0: "self"},
		{file: commandFile, fn: "startLauncher", arg0: "self"},
	}},
	"startPerfAgent": {file: commandFile, calls: []helperCall{{file: attachFile, fn: "attach", arg0: "r.spec.PerfAgent"}}},
}

// advice is the advice of a finding in a file held to rule.
func (rule execRule) advice() string {
	var calls []string
	for _, c := range rule.calls {
		calls = append(calls, c.form+" in "+c.fn)
	}
	return "only " + strings.Join(calls, " and ") + " may start a process"
}

// prctlOptions are the prctl options launchFile may pass to RawSyscall,
// with the value the kernel gives each: the launcher's no_new_privs and
// the reaper's child subreaper.
var prctlOptions = map[string]struct {
	value  string
	kernel string
}{
	"prSetNoNewPrivs":     {"38", "PR_SET_NO_NEW_PRIVS"},
	"prSetChildSubreaper": {"36", "PR_SET_CHILD_SUBREAPER"},
}

// shellNames are programs the server never runs: a string naming one, or a
// path ending in one, is a finding where shells are checked.
var shellNames = []string{"sh", "bash", "dash", "zsh", "sudo", "pkexec", "su", "doas"}

// clientMethods are names that dial, resolve or listen whatever their
// receiver, so a dialer or client reached without naming its type is
// caught too, and so is a package function of the same name.
var clientMethods = []string{
	"DialContext", "DialTLSContext", "RoundTrip", "LookupIPAddr", "LookupNetIP",
	"ListenAndServe", "ListenAndServeTLS", "ServeTLS",
}

// clientPackages are standard packages that exist to make requests; no
// package of the server may import one. processPackages start a process
// (go/build and go/importer run the go command) or load code (plugin); no
// package of the server may import or link one.
var (
	clientPackages = []string{
		"net/smtp", "net/rpc", "net/rpc/jsonrpc", "net/http/httputil", "net/http/httptest",
		"net/http/httptrace", "net/http/cookiejar", "net/http/cgi", "net/http/fcgi",
		"net/textproto", "log/syslog",
	}
	processPackages = []string{"go/build", "go/importer", "plugin"}
)

// The findings about exec.Cmd values, which only exec.Command and
// exec.CommandContext make.
const (
	cmdLiteral = "builds an exec.Cmd literal: only exec.Command and exec.CommandContext may make one"
	cmdPath    = "sets exec.Cmd.Path: only exec.Command's and exec.CommandContext's argument may name the program"
)

// cmdFields are the exec.Cmd fields only the files allowed os/exec may
// set; Path is never set.
var cmdFields = []string{"Path", "Args", "Env", "Dir"}

// scanned is what scanNetworking found in a set of files.
type scanned struct {
	findings []string
	// listens are the listening calls in the allowed file, as
	// "pkg.Name"; exec has each allowed file seen importing os/exec.
	listens []string
	exec    map[string]bool
	// commands are the allowed os/exec calls seen in each file allowed
	// os/exec, as "func: form", and launches the allowed system calls
	// seen in the launch file, in source order.
	commands map[string][]string
	launches []string
	// helpers are the reviewed helper calls seen, as "fn: helper(arg0, …)".
	helpers []string
}

// typeUnit is one package to type-check: its files for one target, the
// target's GOARCH, and the export data its imports are read from.
type typeUnit struct {
	target, arch, path string
	files              []string
	exports            *exportFiles
}

// exportFiles finds the compiler's export data of a package for one
// target: from the listing it was filled from, or else from go list
// -export run with env.
type exportFiles struct {
	env   []string
	mu    sync.Mutex
	files map[string]string // import path -> export data file
}

func (e *exportFiles) open(path string) (io.ReadCloser, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.files[path]; !ok {
		pkgs, err := goList(".", path, true, e.env)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			e.files[p.ImportPath] = p.Export
		}
	}
	if e.files[path] == "" {
		return nil, fmt.Errorf("no export data for %s", path)
	}
	return os.Open(e.files[path])
}

// hostExports are the export data of the host target, for the fixtures.
var hostExports = sync.OnceValue(func() *exportFiles {
	return &exportFiles{
		env:   []string{"GOOS=" + runtime.GOOS, "GOARCH=" + runtime.GOARCH, "CGO_ENABLED=0"},
		files: map[string]string{},
	}
})

// fixtureUnit type-checks fixture files as one package for the host.
func fixtureUnit(files ...string) typeUnit {
	return typeUnit{
		target: runtime.GOOS + "/" + runtime.GOARCH, arch: runtime.GOARCH,
		path: "example.com/x", files: files, exports: hostExports(),
	}
}

// located is one finding.
type located struct {
	file string
	line int
	msg  string
}

// scanner holds one scan's parsed files and what it found.
type scanner struct {
	t       *testing.T
	fset    *token.FileSet
	allow   allowedFiles
	display func(string) string
	parsed  map[string]*ast.File
	imps    map[*exportFiles]types.Importer
	// allowed are the selectors of the reviewed forms the type-checked
	// pass found: the syntax pass skips them, and reports every other use
	// of the same names.
	allowed map[*ast.SelectorExpr]bool
	found   []located
	out     scanned
}

func (s *scanner) report(n ast.Node, format string, args ...any) {
	p := s.fset.Position(n.Pos())
	l := located{s.display(p.Filename), p.Line, fmt.Sprintf(format, args...)}
	if !slices.Contains(s.found, l) {
		s.found = append(s.found, l)
	}
}

func (s *scanner) parse(file string) *ast.File {
	if f, ok := s.parsed[file]; ok {
		return f
	}
	f, err := parser.ParseFile(s.fset, file, nil, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		s.t.Fatalf("parse %s: %v", file, err)
	}
	s.parsed[file] = f
	return f
}

// scanNetworking reads every file and reports each use of a client name
// or client method, an import of a client or process package, a listening
// call outside allow.listen and an os/exec import outside allow.exec. A
// dot import of a networking package or of os/exec is a finding too, as
// its names could not be told apart, and so are cgo and go:linkname,
// which reach code the scan cannot read. Import names are resolved per
// file.
//
// Each of units is type-checked, and in its files names are resolved to
// the objects they denote. An exec.Cmd literal is a finding anywhere, and
// so is setting a Cmd's Path, or its Args, Env or Dir outside allow.exec.
// In a file of allow.exec, os/exec offers only its rule's types and calls,
// each call once and with its arguments bound as the rule says. In
// allow.launch, syscall.Exec may be called once and syscall.RawSyscall
// only as RawSyscall(syscall.SYS_PRCTL, option, …) for each of
// prctlOptions once, the option a package-level constant of the kernel's
// value declared in that file. Any other use of those names, and every use
// in a file no unit type-checks, is the finding it is everywhere else.
func scanNetworking(t *testing.T, files []string, units []typeUnit, display func(string) string, allow allowedFiles) scanned {
	t.Helper()
	s := &scanner{
		t: t, fset: token.NewFileSet(), allow: allow, display: display,
		parsed: map[string]*ast.File{}, imps: map[*exportFiles]types.Importer{},
		allowed: map[*ast.SelectorExpr]bool{},
		out:     scanned{exec: map[string]bool{}, commands: map[string][]string{}},
	}
	for _, u := range units {
		s.check(u)
	}
	for _, file := range files {
		s.syntax(file)
	}
	slices.SortStableFunc(s.found, func(a, b located) int {
		return cmp.Or(strings.Compare(a.file, b.file), cmp.Compare(a.line, b.line))
	})
	for _, l := range s.found {
		s.out.findings = append(s.out.findings, fmt.Sprintf("%s:%d: %s", l.file, l.line, l.msg))
	}
	return s.out
}

// check type-checks u and applies the rules that need resolved names.
func (s *scanner) check(u typeUnit) {
	var files []*ast.File
	for _, f := range u.files {
		files = append(files, s.parse(f))
	}
	imp, ok := s.imps[u.exports]
	if !ok {
		imp = importer.ForCompiler(s.fset, "gc", u.exports.open)
		s.imps[u.exports] = imp
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Scopes:     map[ast.Node]*types.Scope{},
	}
	conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", u.arch)}
	pkg, err := conf.Check(u.path, s.fset, files, info)
	if err != nil {
		s.t.Fatalf("type-check %s for %s: %v", u.path, u.target, err)
	}
	// The reviewed forms are allowed once each per type-check.
	seen := map[string]bool{}
	for i, f := range files {
		name := s.display(u.files[i])
		s.cmdUses(f, info, name)
		if rule, ok := s.allow.exec[name]; ok {
			s.execCalls(f, info, name, rule, seen)
		}
		if name == s.allow.launch {
			s.launchCalls(f, info, pkg, u.files[i], seen)
		}
	}
	for _, helper := range slices.Sorted(maps.Keys(s.allow.helpers)) {
		rule := s.allow.helpers[helper]
		fn, ok := pkg.Scope().Lookup(helper).(*types.Func)
		if !ok || s.display(s.fset.Position(fn.Pos()).Filename) != rule.file {
			continue
		}
		s.helperCalls(files, u.files, info, fn, rule, seen)
	}
}

// helperCalls allows the first call of fn by each of rule's callers whose
// first argument is bound as the rule says, and reports every other
// reference to fn in files, a func value included.
func (s *scanner) helperCalls(files []*ast.File, paths []string, info *types.Info, fn *types.Func, rule helperRule, seen map[string]bool) {
	allowed := map[*ast.Ident]bool{}
	var only []string
	for _, c := range rule.calls {
		only = append(only, fmt.Sprintf("%s(%s, …) once in %s (%s)", fn.Name(), c.arg0, c.fn, c.file))
	}
	for i, f := range files {
		name := s.display(paths[i])
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				id, ok := ast.Unparen(call.Fun).(*ast.Ident)
				if !ok || info.Uses[id] != fn {
					return true
				}
				arg0 := types.ExprString(call.Args[0])
				i := slices.IndexFunc(rule.calls, func(c helperCall) bool { return c.file == name && c.fn == fd.Name.Name && c.arg0 == arg0 })
				desc := fd.Name.Name + ": " + fn.Name() + "(" + arg0 + ", …)"
				if i < 0 || seen[name+": "+desc] {
					return true
				}
				allowed[id] = true
				if why := ownArgument(fd, info, call.Args[0]); why != "" {
					s.report(call, "%s in %s: %s", types.ExprString(call), fd.Name.Name, why)
					return true
				}
				seen[name+": "+desc] = true
				if !slices.Contains(s.out.helpers, desc) {
					s.out.helpers = append(s.out.helpers, desc)
				}
				return true
			})
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && info.Uses[id] == fn && !allowed[id] {
				s.report(id, "references %s: only %s may", fn.Name(), strings.Join(only, " and "))
			}
			return true
		})
	}
}

// ownArgument says why arg, a variable or a field of one, is not fd's own
// receiver or parameter left as it was given, or "".
func ownArgument(fd *ast.FuncDecl, info *types.Info, arg ast.Expr) string {
	root := ast.Unparen(arg)
	for {
		sel, ok := root.(*ast.SelectorExpr)
		if !ok {
			break
		}
		root = ast.Unparen(sel.X)
	}
	id, ok := root.(*ast.Ident)
	v, isVar := info.Uses[id].(*types.Var)
	if !ok || !isVar {
		return fmt.Sprintf("%s is not a variable", types.ExprString(arg))
	}
	sig := info.Defs[fd.Name].(*types.Func).Signature()
	own := slices.Collect(sig.Params().Variables())
	if sig.Recv() != nil {
		own = append(own, sig.Recv())
	}
	switch {
	case !slices.Contains(own, v):
		return fmt.Sprintf("%s is not a receiver or parameter of %s", id.Name, fd.Name.Name)
	case assigned(fd.Body, info, v):
		return fmt.Sprintf("%s is assigned again, or its address taken, in %s", id.Name, fd.Name.Name)
	}
	return ""
}

// cmdUses reports exec.Cmd literals, and the Cmd fields f sets that its
// file may not.
func (s *scanner) cmdUses(f *ast.File, info *types.Info, name string) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			// A literal of Cmd's struct, whatever the type is named; its
			// keys, being Cmd's fields, can only belong to that struct.
			if t := info.TypeOf(n); isExecCmd(t) || hasCmdStruct(t) {
				s.report(n, cmdLiteral)
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				s.cmdField(info, lhs, name)
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				s.cmdField(info, n.X, name)
			}
		}
		return true
	})
}

// cmdField reports e, an assigned expression or one whose address is
// taken, when it is (an element of) a Cmd field the file name may not set.
func (s *scanner) cmdField(info *types.Info, e ast.Expr, name string) {
	for {
		if ix, ok := ast.Unparen(e).(*ast.IndexExpr); ok {
			e = ix.X
			continue
		}
		break
	}
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return
	}
	selection := info.Selections[sel]
	if selection == nil || selection.Kind() != types.FieldVal {
		return
	}
	field := selection.Obj().(*types.Var)
	switch {
	case !slices.Contains(cmdFields, field.Name()) || !isCmdField(field):
	case field.Name() == "Path":
		s.report(sel, cmdPath)
	case !s.isExecFile(name):
		s.report(sel, "sets exec.Cmd.%s outside %s", field.Name(), strings.Join(slices.Sorted(maps.Keys(s.allow.exec)), " and "))
	}
}

// isExecFile reports whether the file displayed as name is allowed os/exec.
func (s *scanner) isExecFile(name string) bool {
	_, ok := s.allow.exec[name]
	return ok
}

// isExecCmd reports whether t is os/exec's Cmd or a pointer to it.
func isExecCmd(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "os/exec" && n.Obj().Name() == "Cmd"
}

// hasCmdStruct reports whether t, or what it points to, has exec.Cmd's
// struct as its underlying type, as a type defined from exec.Cmd does.
func hasCmdStruct(t types.Type) bool {
	if t == nil {
		return false
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	return ok && st.NumFields() > 0 && isCmdField(st.Field(0))
}

// isCmdField reports whether field is one of os/exec Cmd's own fields.
func isCmdField(field *types.Var) bool {
	if field.Pkg() == nil || field.Pkg().Path() != "os/exec" {
		return false
	}
	tn, ok := field.Pkg().Scope().Lookup("Cmd").(*types.TypeName)
	if !ok {
		return false
	}
	st, ok := tn.Type().Underlying().(*types.Struct)
	return ok && slices.Contains(slices.Collect(st.Fields()), field)
}

// pkgSelector reports whether e is a name qualified by an import of the
// package with import path p, the qualifier resolved by go/types.
func pkgSelector(info *types.Info, e ast.Expr, p string) (*ast.SelectorExpr, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil, false
	}
	pn, ok := info.Uses[x].(*types.PkgName)
	return sel, ok && pn.Imported().Path() == p
}

// execForm is call written with os/exec named exec, when call is a call
// of an os/exec function.
func execForm(info *types.Info, call *ast.CallExpr) (string, *ast.SelectorExpr, bool) {
	sel, ok := pkgSelector(info, call.Fun, "os/exec")
	if !ok {
		return "", nil, false
	}
	args := strings.TrimPrefix(types.ExprString(call), types.ExprString(call.Fun))
	return "exec." + sel.Sel.Name + args, sel, true
}

// execCalls finds, in f's functions, the first call of each of rule's
// allowed calls, marks its selector allowed and records it; one whose
// arguments are not bound as the rule says is a finding.
func (s *scanner) execCalls(f *ast.File, info *types.Info, name string, rule execRule, seen map[string]bool) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			form, sel, ok := execForm(info, call)
			if !ok {
				return true
			}
			i := slices.IndexFunc(rule.calls, func(c allowedCall) bool { return c.fn == fd.Name.Name && c.form == form })
			desc := fd.Name.Name + ": " + form
			if i < 0 || seen[name+": "+desc] {
				return true
			}
			s.allowed[sel] = true
			if why := bindings(fd, info, call, rule.calls[i]); why != "" {
				s.report(call, "%s in %s: %s", form, fd.Name.Name, why)
				return true
			}
			seen[name+": "+desc] = true
			if !slices.Contains(s.out.commands[name], desc) {
				s.out.commands[name] = append(s.out.commands[name], desc)
			}
			return true
		})
	}
}

// bindings says how call's arguments break c's binding rules, or "".
func bindings(fd *ast.FuncDecl, info *types.Info, call *ast.CallExpr, c allowedCall) string {
	fn := info.Defs[fd.Name].(*types.Func)
	scope := info.Scopes[fd.Type]
	var why string
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if why != "" || !ok {
				return why == ""
			}
			v, ok := info.Uses[id].(*types.Var)
			switch {
			case !ok:
			case slices.Contains(c.params, id.Name) && !slices.Contains(slices.Collect(fn.Signature().Params().Variables()), v):
				why = fmt.Sprintf("%s is not a parameter of %s", id.Name, fd.Name.Name)
			case v.Parent() != scope:
				why = fmt.Sprintf("%s is not a variable of %s's own scope", id.Name, fd.Name.Name)
			case assigned(fd.Body, info, v):
				why = fmt.Sprintf("%s is assigned again, or its address taken, in %s", id.Name, fd.Name.Name)
			case c.defs[id.Name] != "" && !definedBy(fd.Body, info, v, c.defs[id.Name]):
				why = fmt.Sprintf("%s is not defined by %s", id.Name, c.defs[id.Name])
			}
			return why == ""
		})
		if why != "" {
			break
		}
	}
	return why
}

// assigned reports whether body assigns v after its declaration, or takes
// its address.
func assigned(body ast.Node, info *types.Info, v *types.Var) bool {
	is := func(e ast.Expr) bool {
		id, ok := ast.Unparen(e).(*ast.Ident)
		return ok && info.Uses[id] == v
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			found = found || slices.ContainsFunc(n.Lhs, is)
		case *ast.IncDecStmt:
			found = found || is(n.X)
		case *ast.UnaryExpr:
			found = found || n.Op == token.AND && is(n.X)
		case *ast.RangeStmt:
			found = found || n.Tok == token.ASSIGN && (is(n.Key) || is(n.Value))
		}
		return !found
	})
	return found
}

// definedBy reports whether v is declared, alone or first, by a := of the
// os/exec call written as form.
func definedBy(body ast.Node, info *types.Info, v *types.Var, form string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE || len(as.Rhs) != 1 {
			return !found
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		call, isCall := as.Rhs[0].(*ast.CallExpr)
		if ok && isCall && info.Defs[id] == v {
			got, _, ok := execForm(info, call)
			found = ok && got == form
		}
		return !found
	})
	return found
}

// launchCalls finds, in f, the first syscall.Exec call and the first
// RawSyscall(syscall.SYS_PRCTL, option, …) of each prctlOptions option,
// marks their selectors allowed and records them in source order. A prctl
// call whose option is not a package-level constant of the kernel's value
// declared in f is a finding, and is not allowed.
func (s *scanner) launchCalls(f *ast.File, info *types.Info, pkg *types.Package, file string, seen map[string]bool) {
	record := func(desc string) {
		seen[desc] = true
		if !slices.Contains(s.out.launches, desc) {
			s.out.launches = append(s.out.launches, desc)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := pkgSelector(info, call.Fun, "syscall")
		switch {
		case !ok:
		case sel.Sel.Name == "Exec" && !seen["syscall.Exec"]:
			s.allowed[sel] = true
			record("syscall.Exec")
		case sel.Sel.Name == "RawSyscall" && len(call.Args) == 4 && isConst(info, call.Args[0], "syscall", "SYS_PRCTL"):
			opt, ok := call.Args[1].(*ast.Ident)
			if !ok {
				return true
			}
			want, known := prctlOptions[opt.Name]
			desc := "syscall.RawSyscall(SYS_PRCTL, " + opt.Name + ")"
			if !known || seen[desc] {
				return true
			}
			s.allowed[sel] = true
			c, ok := info.Uses[opt].(*types.Const)
			if !ok || c.Parent() != pkg.Scope() || s.fset.Position(c.Pos()).Filename != file || c.Val().ExactString() != want.value {
				s.report(call, "%s is not the package-level constant %s (%s) of %s", opt.Name, want.value, want.kernel, s.display(file))
				return true
			}
			record(desc)
		}
		return true
	})
}

// isConst reports whether e denotes the constant name of the package with
// import path p.
func isConst(info *types.Info, e ast.Expr, p, name string) bool {
	id, ok := e.(*ast.Ident)
	if sel, isSel := e.(*ast.SelectorExpr); isSel {
		id, ok = sel.Sel, true
	}
	if !ok {
		return false
	}
	c, ok := info.Uses[id].(*types.Const)
	return ok && c.Pkg() != nil && c.Pkg().Path() == p && c.Name() == name
}

// syntax applies the rules that need no types to file.
func (s *scanner) syntax(file string) {
	f := s.parse(file)
	name := s.display(file)
	rule, execFile := s.allow.exec[name]
	local := map[string]string{} // import name in this file -> path
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			s.t.Fatalf("%s: import %s: %v", name, imp.Path.Value, err)
		}
		switch {
		case slices.Contains(clientPackages, p):
			s.report(imp, "imports %s", p)
		case slices.Contains(processPackages, p):
			s.report(imp, "imports %s, which can start a process or load code", p)
		case p == "C":
			s.report(imp, "imports C: cgo the scan cannot read")
		case p == "os/exec" && execFile:
			s.out.exec[name] = true
		case p == "os/exec":
			s.report(imp, "imports os/exec outside %s", strings.Join(slices.Sorted(maps.Keys(s.allow.exec)), " and "))
		}
		if first, _, _ := strings.Cut(p, "/"); s.allow.covered != nil && strings.Contains(first, ".") && !s.allow.covered[p] {
			s.report(imp, "imports %s, which no scan target links, so the scan does not read it", p)
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
			if client || listen || p == "os/exec" {
				s.report(imp, "dot-imports %s, so its names cannot be checked", p)
			}
		default:
			local[n] = p
		}
	}
	for _, g := range f.Comments {
		for _, c := range g.List {
			if strings.HasPrefix(c.Text, "//go:linkname") {
				s.report(c, "has a go:linkname directive")
			}
		}
	}
	// qualified resolves a selector's package name to an import of f.
	qualified := func(e ast.Expr) (string, *ast.SelectorExpr, bool) {
		sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
		if !ok {
			return "", nil, false
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", sel, false
		}
		p, ok := local[x.Name]
		return p, sel, ok
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind == token.STRING && strings.HasPrefix(name, s.allow.noShell) {
				if v, err := strconv.Unquote(n.Value); err == nil && slices.Contains(shellNames, path.Base(v)) {
					s.report(n, "has the string %q: the server never runs a shell, sudo or pkexec", v)
				}
			}
			return true
		case *ast.CompositeLit:
			if p, sel, ok := qualified(n.Type); ok && p == "os/exec" && sel.Sel.Name == "Cmd" {
				s.report(n, cmdLiteral)
			}
			return true
		case *ast.SelectorExpr:
		default:
			return true
		}
		p, sel, ok := qualified(n.(*ast.SelectorExpr))
		if s.allowed[sel] {
			return true
		}
		if ok {
			qual := p + "." + sel.Sel.Name
			switch {
			case p == "os/exec" && execFile && !slices.Contains(rule.types, sel.Sel.Name):
				s.report(sel, "uses %s: %s", qual, rule.advice())
			case slices.Contains(listenNames[p], sel.Sel.Name):
				if name == s.allow.listen {
					s.out.listens = append(s.out.listens, qual)
				} else {
					s.report(sel, "uses %s outside %s", qual, s.allow.listen)
				}
			case clientName(p, sel.Sel.Name), slices.Contains(clientMethods, sel.Sel.Name):
				s.report(sel, "uses %s", qual)
			}
			return true
		}
		if slices.Contains(clientMethods, sel.Sel.Name) {
			s.report(sel, "calls a method named %s", sel.Sel.Name)
		}
		return true
	})
}

// listedPackage is one package of go list -deps: its files the scan reads
// or type-checks, the files that are not Go (cgo, C, C++, Objective-C,
// Fortran, assembly and system objects) and, with -export, its export
// data.
type listedPackage struct {
	ImportPath, Dir, Export                                     string
	Standard                                                    bool
	GoFiles, CgoFiles, CFiles, CXXFiles, MFiles, FFiles, SFiles []string
	SysoFiles, IgnoredOtherFiles                                []string
}

// nativeKinds names listedPackage's non-Go file lists, in nativeCode's
// order.
var nativeKinds = []string{"cgo", "C", "C++", "Objective-C", "Fortran", "assembly", "syso"}

// goList runs go list -deps on pattern, one or more package patterns
// separated by spaces, in dir, with env added to the environment, and
// returns every package, the standard ones included.
// With export the packages are compiled and their export data listed.
func goList(dir, pattern string, export bool, env []string) ([]listedPackage, error) {
	args := []string{"list", "-buildvcs=false", "-deps",
		"-json=ImportPath,Dir,Export,Standard,GoFiles,CgoFiles,CFiles,CXXFiles,MFiles,FFiles,SFiles,SysoFiles,IgnoredOtherFiles"}
	if export {
		args = append(args, "-export")
	}
	// A pattern of several packages lists them all, as go list does.
	cmd := exec.Command("go", append(args, strings.Fields(pattern)...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s (%s): %w: %s", pattern, strings.Join(env, " "), err, stderr.String())
	}
	var pkgs []listedPackage
	for dec := json.NewDecoder(strings.NewReader(string(out))); ; {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return pkgs, nil
		} else if err != nil {
			return nil, fmt.Errorf("go list %s: %w", pattern, err)
		}
		if p.Dir == "" {
			return nil, fmt.Errorf("go list %s: no directory for %s", pattern, p.ImportPath)
		}
		pkgs = append(pkgs, p)
	}
}

// listPackages is goList for a test.
func listPackages(t *testing.T, dir, pattern string, export bool, env ...string) []listedPackage {
	t.Helper()
	pkgs, err := goList(dir, pattern, export, env)
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

// nativeCode says which files of p are not Go, or "" when all are: code
// the source scan cannot read.
func nativeCode(p listedPackage) string {
	var kinds []string
	for i, files := range [][]string{p.CgoFiles, p.CFiles, p.CXXFiles, p.MFiles, p.FFiles, p.SFiles, p.SysoFiles} {
		if len(files) > 0 {
			kinds = append(kinds, fmt.Sprintf("%d %s", len(files), nativeKinds[i]))
		}
	}
	return strings.Join(kinds, ", ")
}

// otherNative are the assembly and system object files of p that the
// listed target leaves out: code another target builds, and the scan
// cannot read.
func otherNative(p listedPackage) []string {
	return slices.DeleteFunc(slices.Clone(p.IgnoredOtherFiles), func(f string) bool {
		return !slices.Contains([]string{".s", ".S", ".syso"}, filepath.Ext(f))
	})
}

// scanTargets are the GOOS/GOARCH pairs whose packages the scan reads.
var scanTargets = []string{"linux/amd64", "linux/arm64", "darwin/arm64"}

// targetListing is what listTargets found over scanTargets.
type targetListing struct {
	// dirs are the non-standard packages linked for any target, by import
	// path; units type-check each for each target.
	dirs  map[string]string
	units []typeUnit
	// problems are the packages with code the scan cannot read, and the
	// process packages linked, once each.
	problems []string
}

// listTargets lists pattern's dependencies in dir for every scan target,
// with env added: once with cgo enabled, so cgo files are counted rather
// than ignored, and once without, for the export data the type-check reads.
func listTargets(t *testing.T, dir, pattern string, env ...string) targetListing {
	t.Helper()
	l := targetListing{dirs: map[string]string{}}
	note := func(format string, args ...any) {
		if m := fmt.Sprintf(format, args...); !slices.Contains(l.problems, m) {
			l.problems = append(l.problems, m)
		}
	}
	for _, target := range scanTargets {
		goos, goarch, _ := strings.Cut(target, "/")
		tenv := append(slices.Clone(env), "GOOS="+goos, "GOARCH="+goarch)
		for _, p := range listPackages(t, dir, pattern, false, append(tenv, "CGO_ENABLED=1")...) {
			if p.Standard {
				if slices.Contains(processPackages, p.ImportPath) {
					note("%s links %s on %s, which can start a process or load code", pattern, p.ImportPath, target)
				}
				continue
			}
			l.dirs[p.ImportPath] = p.Dir
			if native := nativeCode(p); native != "" {
				note("%s has files the scan cannot read on %s: %s", p.ImportPath, target, native)
			}
			if other := otherNative(p); len(other) > 0 {
				note("%s has assembly or object files for other targets, which the scan cannot read: %s", p.ImportPath, strings.Join(other, ", "))
			}
		}
		exports := &exportFiles{env: append(tenv, "CGO_ENABLED=0"), files: map[string]string{}}
		typed := listPackages(t, dir, pattern, true, exports.env...)
		for _, p := range typed {
			exports.files[p.ImportPath] = p.Export
		}
		for _, p := range typed {
			if p.Standard {
				continue
			}
			u := typeUnit{target: target, arch: goarch, path: p.ImportPath, exports: exports}
			for _, f := range p.GoFiles {
				u.files = append(u.files, filepath.Join(p.Dir, f))
			}
			l.units = append(l.units, u)
		}
	}
	return l
}

// covered is the set of l's packages.
func (l targetListing) covered() map[string]bool {
	out := map[string]bool{}
	for p := range l.dirs {
		out[p] = true
	}
	return out
}

// scanFiles are every non-test Go file of dirs, whatever its build
// constraints.
func scanFiles(t *testing.T, dirs map[string]string) []string {
	t.Helper()
	var files []string
	for _, dir := range slices.Sorted(maps.Values(dirs)) {
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
	return files
}

// scanPattern is what the network scan reads: the binary, and the
// packages that serve the live page over HTTP, which the binary does not
// link, so the one listening call stays under the scan.
const scanPattern = ". ../../internal/livehttp ../../internal/perfpage"

// TestNoClientNetworkingInTheServer reads the source of every non-standard
// package scanPattern links on any scan target, internal/livehttp's and
// internal/perfpage's included, and finds no dial, name lookup, HTTP
// client, non-TCP socket or raw system call, no listening call but the one in
// internal/livehttp/listen.go, which checks the address is loopback first,
// no os/exec outside the git probe and capture's exec_linux.go, each held
// to its reviewed calls, no hand-built or retargeted exec.Cmd, and in
// capture no raw system call but the launcher's prctl and exec.
// None of those packages has code the scan cannot read: no cgo, assembly
// or object files for any target, and no go:linkname; and the binary
// links none of the process packages.
func TestNoClientNetworkingInTheServer(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	listed := listTargets(t, ".", scanPattern)
	for _, p := range listed.problems {
		t.Error(p)
	}
	// Every non-test file, whatever its build constraints, so a file for
	// another target or a custom tag is read by the name rules too.
	files := scanFiles(t, listed.dirs)
	// The scanned set is the binary's, not this test's: go list -deps
	// without -test leaves test files and test-only imports out. This file
	// imports go/importer, so the process-package check above would also
	// fail if test dependencies were ever listed.
	for _, u := range listed.units {
		if i := slices.IndexFunc(u.files, func(f string) bool { return strings.HasSuffix(f, "_test.go") }); i >= 0 {
			t.Errorf("the scan type-checks the test file %s", u.files[i])
		}
	}
	for _, want := range []string{
		"github.com/dpsoft/perf-agent/internal/capture",
		"github.com/dpsoft/perf-agent/internal/livehttp",
		"github.com/dpsoft/perf-agent/internal/perfpage",
		"github.com/dpsoft/perf-agent/internal/perfmcp",
		"github.com/dpsoft/perf-agent/internal/mcpstdio",
		"github.com/dpsoft/perf-agent/internal/sourceroot",
		"github.com/google/pprof/profile",
	} {
		if _, ok := listed.dirs[want]; !ok {
			t.Errorf("the scan does not cover %s; it covers %v", want, slices.Sorted(maps.Keys(listed.dirs)))
		}
	}
	display := func(file string) string {
		if rel, err := filepath.Rel(root, file); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		return file
	}
	got := scanNetworking(t, files, listed.units, display, allowedFiles{
		listen: listenFile, exec: execRules, launch: launchFile, noShell: shellFreeDir,
		helpers: helperRules, covered: listed.covered(),
	})
	for _, f := range got.findings {
		t.Error(f)
	}
	// The one listening call, each os/exec import, the reviewed process
	// starts and the launcher's three system calls are found, so the scan
	// reads and type-checks the files that hold them and resolves their
	// imports.
	if !slices.Equal(got.listens, []string{"net.Listen"}) {
		t.Errorf("listening calls in %s = %v, want exactly net.Listen", listenFile, got.listens)
	}
	for _, file := range slices.Sorted(maps.Keys(execRules)) {
		if !got.exec[file] {
			t.Errorf("the scan did not see %s import os/exec", file)
		}
		var want []string
		for _, c := range execRules[file].calls {
			want = append(want, c.fn+": "+c.form)
		}
		if !slices.Equal(got.commands[file], want) {
			t.Errorf("os/exec calls in %s = %v, want exactly %v", file, got.commands[file], want)
		}
	}
	if want := []string{"syscall.RawSyscall(SYS_PRCTL, prSetNoNewPrivs)", "syscall.RawSyscall(SYS_PRCTL, prSetChildSubreaper)", "syscall.Exec"}; !slices.Equal(got.launches, want) {
		t.Errorf("system calls in %s = %v, want exactly %v", launchFile, got.launches, want)
	}
	var helpers []string
	for _, h := range slices.Sorted(maps.Keys(helperRules)) {
		for _, c := range helperRules[h].calls {
			helpers = append(helpers, c.fn+": "+h+"("+c.arg0+", …)")
		}
	}
	if got := slices.Sorted(slices.Values(got.helpers)); !slices.Equal(got, slices.Sorted(slices.Values(helpers))) {
		t.Errorf("helper calls = %v, want exactly %v", got, helpers)
	}
	t.Logf("scanned %d files in %d packages, %d type-checks over %v", len(files), len(listed.dirs), len(listed.units), scanTargets)
}

// writeFixtures writes files, named by their slash paths, into a new
// directory and returns the paths of the Go files among them, sorted.
// The fixtures are parsed and type-checked, never built or run.
func writeFixtures(t *testing.T, files map[string]string) []string {
	t.Helper()
	return writeFixturesIn(t, t.TempDir(), files)
}

func writeFixturesIn(t *testing.T, dir string, files map[string]string) []string {
	t.Helper()
	var paths []string
	for name, src := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(p, ".go") {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return paths
}

// checkFindings fails t unless got is exactly want.
func checkFindings(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// gitOnly allows git.go os/exec's ExitError type and nothing else.
var gitOnly = map[string]execRule{"git.go": {types: []string{"ExitError"}}}

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
	got := scanNetworking(t, files, nil, filepath.Base, allowedFiles{listen: "listen.go", exec: gitOnly})
	checkFindings(t, got.findings, []string{
		"a.go:3: uses net/http.Get",
		"b.go:3: uses crypto/tls.Dial",
		"c.go:2: dot-imports net, so its names cannot be checked",
		"d.go:4: calls a method named DialContext",
		"e.go:3: uses net.Listen outside listen.go",
	})
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
		"git.go": "package x\nimport run \"os/exec\"\nvar _ *run.ExitError\n",
	})
	got := scanNetworking(t, files, nil, filepath.Base, allowedFiles{listen: "listen.go", exec: gitOnly})
	checkFindings(t, got.findings, []string{
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
	})
	if !got.exec["git.go"] {
		t.Error("the allowed os/exec import was not seen")
	}
}

// The capture rules as the fixtures name the files.
var captureFixtureAllow = allowedFiles{
	listen:  "listen.go",
	exec:    map[string]execRule{"git.go": gitOnly["git.go"], "exec_linux.go": execRules[commandFile]},
	launch:  "launch_linux.go",
	noShell: "",
}

// The capture package starts processes in two files only: exec_linux.go
// with exactly exec.Command(self) in selfCommand and exec.Command(path) in
// startPerfAgent, and launch_linux.go with syscall.Exec and the two prctl
// calls through RawSyscall. Any other form there is a finding, and so is
// a shell, sudo or pkexec named in a string.
func TestTheNetworkScanSeesTheCaptureExecRules(t *testing.T) {
	allow := captureFixtureAllow
	files := writeFixtures(t, map[string]string{
		"exec_linux.go": "package x\nimport \"os/exec\"\n" +
			"func selfCommand(self, mode string) *exec.Cmd { return exec.Command(self) }\n" +
			"func startPerfAgent(path string) *exec.Cmd { return exec.Command(path) }\n" +
			"func third(p string) *exec.Cmd { return exec.Command(p) }\n" +
			"func look() { _, _ = exec.LookPath(\"git\") }\n" +
			"func withContext() *exec.Cmd { return exec.CommandContext(nil, \"x\") }\n" +
			"var shell = \"sh\"\n" +
			"var _ = []string{\"/bin/bash\", \"sudo\", \"pkexec\"}\n" +
			"func again(self string) *exec.Cmd { return exec.Command(self) }\n" +
			"func startPerfAgent2(path string) *exec.Cmd { return exec.Command(path, \"-x\") }\n" +
			"func startPerfAgent3(p []string) *exec.Cmd { return exec.Command(p[0]) }\n",
		"launch_linux.go": "package x\nimport \"syscall\"\n" +
			"const (\n\tprSetChildSubreaper = 36\n\tprSetNoNewPrivs = 38\n\tprSetDumpable = 4\n)\n" +
			"func a() { syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0) }\n" +
			"func b() { syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0) }\n" +
			"func c() { syscall.RawSyscall(syscall.SYS_GETPID, 0, 0, 0) }\n" +
			"func d() { syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0) }\n" +
			"func e() { _ = syscall.Exec(\"/x\", nil, nil) }\n" +
			"var f = syscall.RawSyscall\n",
		"other.go": "package x\nimport \"syscall\"\nfunc o() { _ = syscall.Exec(\"/x\", nil, nil) }\n",
		"git.go":   "package x\nimport \"os/exec\"\nvar _ *exec.ExitError\n",
	})
	got := scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	only := "only exec.Command(self) in selfCommand and exec.Command(path) in startPerfAgent may start a process"
	checkFindings(t, got.findings, []string{
		"exec_linux.go:5: uses os/exec.Command: " + only,
		"exec_linux.go:6: uses os/exec.LookPath: " + only,
		"exec_linux.go:7: uses os/exec.CommandContext: " + only,
		"exec_linux.go:8: has the string \"sh\": the server never runs a shell, sudo or pkexec",
		"exec_linux.go:9: has the string \"/bin/bash\": the server never runs a shell, sudo or pkexec",
		"exec_linux.go:9: has the string \"sudo\": the server never runs a shell, sudo or pkexec",
		"exec_linux.go:9: has the string \"pkexec\": the server never runs a shell, sudo or pkexec",
		"exec_linux.go:10: uses os/exec.Command: " + only,
		"exec_linux.go:11: uses os/exec.Command: " + only,
		"exec_linux.go:12: uses os/exec.Command: " + only,
		"launch_linux.go:10: uses syscall.RawSyscall",
		"launch_linux.go:11: uses syscall.RawSyscall",
		"launch_linux.go:13: uses syscall.RawSyscall",
		"other.go:3: uses syscall.Exec",
	})
	if want := []string{"selfCommand: exec.Command(self)", "startPerfAgent: exec.Command(path)"}; !slices.Equal(got.commands["exec_linux.go"], want) {
		t.Errorf("commands = %v, want %v", got.commands["exec_linux.go"], want)
	}
	if want := []string{"syscall.RawSyscall(SYS_PRCTL, prSetNoNewPrivs)", "syscall.RawSyscall(SYS_PRCTL, prSetChildSubreaper)", "syscall.Exec"}; !slices.Equal(got.launches, want) {
		t.Errorf("launches = %v, want %v", got.launches, want)
	}
	for f := range allow.exec {
		if !got.exec[f] {
			t.Errorf("the os/exec import of %s was not seen", f)
		}
	}

	// The prctl options must be the constants the kernel defines: a
	// renumbered one would let the allowed form make another call.
	files = writeFixtures(t, map[string]string{
		"launch_linux.go": "package x\nimport \"syscall\"\n" +
			"const prSetNoNewPrivs = 4\n" +
			"var prSetChildSubreaper uintptr = 36\n" +
			"func a() { syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0) }\n" +
			"func b() { syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0) }\n",
	})
	got = scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	checkFindings(t, got.findings, []string{
		"launch_linux.go:5: prSetNoNewPrivs is not the package-level constant 38 (PR_SET_NO_NEW_PRIVS) of launch_linux.go",
		"launch_linux.go:6: prSetChildSubreaper is not the package-level constant 36 (PR_SET_CHILD_SUBREAPER) of launch_linux.go",
	})
}

// Names in the reviewed forms are resolved, not matched: a local constant
// or parameter shadowing a prctl option, a local value named syscall, a
// program variable that is not the function's parameter or is assigned
// before the call are findings.
func TestTheNetworkScanResolvesNames(t *testing.T) {
	allow := captureFixtureAllow
	files := writeFixtures(t, map[string]string{
		"launch_linux.go": "package x\nimport sc \"syscall\"\n" +
			"const (\n\tprSetChildSubreaper = 36\n\tprSetNoNewPrivs = 38\n)\n" +
			"func a() {\n\tconst prSetNoNewPrivs = 4\n\tsc.RawSyscall(sc.SYS_PRCTL, prSetNoNewPrivs, 1, 0)\n}\n" +
			"func b() {\n\tsyscall := struct{ SYS_PRCTL uintptr }{1}\n\tsc.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)\n}\n" +
			"func c(prSetChildSubreaper uintptr) { sc.RawSyscall(sc.SYS_PRCTL, prSetChildSubreaper, 1, 0) }\n" +
			"func d() { sc.RawSyscall(sc.SYS_PRCTL, prSetNoNewPrivs, 1, 0) }\n" +
			// A local constant is not the package-level one, even of the
			// kernel's value.
			"func f() {\n\tconst prSetChildSubreaper = 36\n\tsc.RawSyscall(sc.SYS_PRCTL, prSetChildSubreaper, 1, 0)\n}\n" +
			"func e() { sc.RawSyscall(sc.SYS_PRCTL, prSetChildSubreaper, 1, 0) }\n",
		"exec_linux.go": "package x\nimport \"os/exec\"\n" +
			"func selfCommand(self, mode string) *exec.Cmd {\n\tif mode == \"m\" {\n\t\tself = \"/bin/true\"\n\t}\n\treturn exec.Command(self)\n}\n" +
			"func startPerfAgent(p string) *exec.Cmd {\n\tpath := \"/bin/true\"\n\treturn exec.Command(path)\n}\n",
	})
	got := scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	checkFindings(t, got.findings, []string{
		"exec_linux.go:7: exec.Command(self) in selfCommand: self is assigned again, or its address taken, in selfCommand",
		"exec_linux.go:11: exec.Command(path) in startPerfAgent: path is not a parameter of startPerfAgent",
		"launch_linux.go:9: prSetNoNewPrivs is not the package-level constant 38 (PR_SET_NO_NEW_PRIVS) of launch_linux.go",
		"launch_linux.go:13: uses syscall.RawSyscall",
		"launch_linux.go:15: prSetChildSubreaper is not the package-level constant 36 (PR_SET_CHILD_SUBREAPER) of launch_linux.go",
		"launch_linux.go:19: prSetChildSubreaper is not the package-level constant 36 (PR_SET_CHILD_SUBREAPER) of launch_linux.go",
	})
	if want := []string{"syscall.RawSyscall(SYS_PRCTL, prSetNoNewPrivs)", "syscall.RawSyscall(SYS_PRCTL, prSetChildSubreaper)"}; !slices.Equal(got.launches, want) {
		t.Errorf("launches = %v, want %v", got.launches, want)
	}

	// The options must be declared in the launch file itself.
	files = writeFixtures(t, map[string]string{
		"consts.go":       "package x\nconst (\n\tprSetChildSubreaper = 36\n\tprSetNoNewPrivs = 38\n)\n",
		"launch_linux.go": "package x\nimport \"syscall\"\nfunc a() { syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0) }\n",
	})
	got = scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	checkFindings(t, got.findings, []string{
		"launch_linux.go:3: prSetNoNewPrivs is not the package-level constant 38 (PR_SET_NO_NEW_PRIVS) of launch_linux.go",
	})

	// A parameter shadowed in an inner block, and one whose address is
	// taken, are not the parameter the rule names.
	files = writeFixtures(t, map[string]string{
		"exec_linux.go": "package x\nimport \"os/exec\"\n" +
			"func selfCommand(self, mode string) *exec.Cmd {\n\t{\n\t\tself := \"/bin/true\"\n\t\treturn exec.Command(self)\n\t}\n}\n" +
			"func startPerfAgent(path string) *exec.Cmd {\n\ttarget := &path\n\t*target = \"/bin/true\"\n\treturn exec.Command(path)\n}\n",
	})
	got = scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	checkFindings(t, got.findings, []string{
		"exec_linux.go:6: exec.Command(self) in selfCommand: self is not a parameter of selfCommand",
		"exec_linux.go:12: exec.Command(path) in startPerfAgent: path is assigned again, or its address taken, in startPerfAgent",
	})
	if len(got.commands["exec_linux.go"]) != 0 {
		t.Errorf("commands = %v, want none", got.commands["exec_linux.go"])
	}
}

// An exec.Cmd is made only by exec.Command or exec.CommandContext: a
// literal of the type, through an alias too, is a finding anywhere, and so
// is setting its Path. Its Args, Env and Dir are set only in the files
// allowed os/exec, so a helper's Cmd cannot be retargeted elsewhere.
func TestTheNetworkScanSeesHandBuiltCommands(t *testing.T) {
	allow := captureFixtureAllow
	files := writeFixtures(t, map[string]string{
		"exec_linux.go": "package x\nimport \"os/exec\"\n" +
			"type alias = exec.Cmd\n" +
			"func selfCommand(self, mode string) *exec.Cmd { return exec.Command(self) }\n" +
			"func startPerfAgent(path string) *exec.Cmd {\n\tcmd := exec.Command(path)\n\tcmd.Args = []string{path}\n\tcmd.Env, cmd.Dir = nil, \"/\"\n\treturn cmd\n}\n" +
			"func built() *exec.Cmd { return &exec.Cmd{Path: \"/bin/true\"} }\n" +
			"func aliased() *alias { return &alias{} }\n" +
			"func retarget(c *exec.Cmd) { c.Path = \"/bin/true\" }\n" +
			"type cmdDef exec.Cmd\n" +
			"func defined() *cmdDef { return &cmdDef{Path: \"/bin/true\"} }\n" +
			"func definedEmpty() *cmdDef { return &cmdDef{} }\n",
		"other.go": "package x\n" +
			"func use() {\n\ttarget := selfCommand(\"/bin/true\", \"m\")\n" +
			"\ttarget.Args = []string{\"x\"}\n" +
			"\ttarget.Env = nil\n" +
			"\ttarget.Path = \"/bin/true\"\n" +
			"\ttarget.Args[0] = \"y\"\n" +
			"\tdir := &target.Dir\n" +
			"\t_ = dir\n" +
			"\t_ = target.Run()\n}\n",
	})
	got := scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	checkFindings(t, got.findings, []string{
		"exec_linux.go:11: " + cmdLiteral,
		"exec_linux.go:12: " + cmdLiteral,
		"exec_linux.go:13: " + cmdPath,
		"exec_linux.go:15: " + cmdLiteral,
		"exec_linux.go:16: " + cmdLiteral,
		"other.go:4: sets exec.Cmd.Args outside exec_linux.go and git.go",
		"other.go:5: sets exec.Cmd.Env outside exec_linux.go and git.go",
		"other.go:6: " + cmdPath,
		"other.go:7: sets exec.Cmd.Args outside exec_linux.go and git.go",
		"other.go:8: sets exec.Cmd.Dir outside exec_linux.go and git.go",
	})
	if want := []string{"selfCommand: exec.Command(self)", "startPerfAgent: exec.Command(path)"}; !slices.Equal(got.commands["exec_linux.go"], want) {
		t.Errorf("commands = %v, want %v", got.commands["exec_linux.go"], want)
	}

	// Without types, a literal written with the package name is still seen.
	files = writeFixtures(t, map[string]string{
		"exec_linux.go": "package x\nimport run \"os/exec\"\nvar _ = run.Cmd{}\n",
	})
	got = scanNetworking(t, files, nil, filepath.Base, allow)
	checkFindings(t, got.findings, []string{"exec_linux.go:3: " + cmdLiteral})
}

// The capture fixtures' process-start helpers and their reviewed callers.
var helperFixtureRules = map[string]helperRule{
	"selfCommand": {file: "exec_linux.go", calls: []helperCall{
		{file: "exec_linux.go", fn: "startReaperProcess", arg0: "self"},
		{file: "exec_linux.go", fn: "startLauncher", arg0: "self"},
	}},
	"startPerfAgent": {file: "exec_linux.go", calls: []helperCall{{file: "internal_linux.go", fn: "attach", arg0: "r.spec.PerfAgent"}}},
}

// selfCommand and startPerfAgent are called only from their reviewed
// callers, once each, with the first argument the caller's own receiver
// or parameter (or a field of it), never reassigned; any other reference,
// a func value included, is a finding.
func TestTheNetworkScanPinsTheHelperCalls(t *testing.T) {
	allow := captureFixtureAllow
	allow.helpers = helperFixtureRules
	files := writeFixtures(t, map[string]string{
		"exec_linux.go": "package x\nimport \"os/exec\"\n" +
			"func selfCommand(self, mode string) *exec.Cmd { return exec.Command(self) }\n" +
			"func startPerfAgent(path string) *exec.Cmd { return exec.Command(path) }\n" +
			"func startReaperProcess(self string) *exec.Cmd { return selfCommand(self, \"r\") }\n" +
			"func startLauncher(other string) *exec.Cmd {\n\tself := \"/bin/true\"\n\treturn selfCommand(self, \"l\")\n}\n",
		"internal_linux.go": "package x\n" +
			"type reaper struct{ spec struct{ PerfAgent, Other string } }\n" +
			"func (r *reaper) attach() {\n\t_ = startPerfAgent(r.spec.PerfAgent)\n\t_ = startPerfAgent(r.spec.PerfAgent)\n\t_ = startPerfAgent(r.spec.Other)\n}\n" +
			"func (r *reaper) other() { _ = startPerfAgent(\"/bin/true\") }\n",
		"other.go": "package x\nvar f = selfCommand\nfunc g() { _ = startPerfAgent(\"/bin/true\") }\n",
	})
	got := scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	onlySelf := "references selfCommand: only selfCommand(self, …) once in startReaperProcess (exec_linux.go) and selfCommand(self, …) once in startLauncher (exec_linux.go) may"
	onlyPerf := "references startPerfAgent: only startPerfAgent(r.spec.PerfAgent, …) once in attach (internal_linux.go) may"
	checkFindings(t, got.findings, []string{
		`exec_linux.go:8: selfCommand(self, "l") in startLauncher: self is not a receiver or parameter of startLauncher`,
		"internal_linux.go:5: " + onlyPerf,
		"internal_linux.go:6: " + onlyPerf,
		"internal_linux.go:8: " + onlyPerf,
		"other.go:2: " + onlySelf,
		"other.go:3: " + onlyPerf,
	})
	if want := []string{"startReaperProcess: selfCommand(self, …)", "attach: startPerfAgent(r.spec.PerfAgent, …)"}; !slices.Equal(got.helpers, want) {
		t.Errorf("helpers = %v, want %v", got.helpers, want)
	}

	// A receiver reassigned before the call is not the caller's own.
	files = writeFixtures(t, map[string]string{
		"exec_linux.go": "package x\nimport \"os/exec\"\n" +
			"func startPerfAgent(path string) *exec.Cmd { return exec.Command(path) }\n",
		"internal_linux.go": "package x\n" +
			"type reaper struct{ spec struct{ PerfAgent string } }\n" +
			"func (r *reaper) attach() {\n\tr = &reaper{}\n\t_ = startPerfAgent(r.spec.PerfAgent)\n}\n",
	})
	got = scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	checkFindings(t, got.findings, []string{
		"internal_linux.go:5: startPerfAgent(r.spec.PerfAgent) in attach: r is assigned again, or its address taken, in attach",
	})
}

// go/build, go/importer and plugin may not be imported, nor os/exec dot
// imported even where it is allowed; the ptrace wrappers, the other raw
// call forms and the credential and namespace calls are denied.
func TestTheNetworkScanDeniesMoreProcessStarts(t *testing.T) {
	files := writeFixtures(t, map[string]string{
		"p.go": "package x\nimport \"go/build\"\nvar _ = build.Default\n",
		"q.go": "package x\nimport (\n\t_ \"go/importer\"\n\t_ \"plugin\"\n)\n",
		"r.go": "package x\nimport sc \"syscall\"\n" +
			"var _ = sc.AllThreadsSyscall\nvar _ = sc.AllThreadsSyscall6\n" +
			"var _ = sc.PtraceAttach\nvar _ = sc.PtracePokeText\n" +
			"var _ = sc.Mount\nvar _ = sc.Unshare\n" +
			"var _ = sc.Setuid\nvar _ = sc.Setgid\nvar _ = sc.Setresuid\nvar _ = sc.Setresgid\n" +
			"var _ = sc.Setreuid\nvar _ = sc.Setregid\nvar _ = sc.Setgroups\nvar _ = sc.Setfsuid\nvar _ = sc.Setfsgid\nvar _ = sc.Chroot\n",
		"s.go":   "package x\nimport u \"golang.org/x/sys/unix\"\nvar _ = u.SyscallNoError\nvar _ = u.RawSyscallNoError\nvar _ = u.PtraceSetRegs\nvar _ = u.PivotRoot\n",
		"git.go": "package x\nimport . \"os/exec\"\nvar _ *ExitError\n",
	})
	got := scanNetworking(t, files, nil, filepath.Base, allowedFiles{listen: "listen.go", exec: gitOnly})
	checkFindings(t, got.findings, []string{
		"git.go:2: dot-imports os/exec, so its names cannot be checked",
		"p.go:2: imports go/build, which can start a process or load code",
		"q.go:3: imports go/importer, which can start a process or load code",
		"q.go:4: imports plugin, which can start a process or load code",
		"r.go:3: uses syscall.AllThreadsSyscall",
		"r.go:4: uses syscall.AllThreadsSyscall6",
		"r.go:5: uses syscall.PtraceAttach",
		"r.go:6: uses syscall.PtracePokeText",
		"r.go:7: uses syscall.Mount",
		"r.go:8: uses syscall.Unshare",
		"r.go:9: uses syscall.Setuid",
		"r.go:10: uses syscall.Setgid",
		"r.go:11: uses syscall.Setresuid",
		"r.go:12: uses syscall.Setresgid",
		"r.go:13: uses syscall.Setreuid",
		"r.go:14: uses syscall.Setregid",
		"r.go:15: uses syscall.Setgroups",
		"r.go:16: uses syscall.Setfsuid",
		"r.go:17: uses syscall.Setfsgid",
		"r.go:18: uses syscall.Chroot",
		"s.go:3: uses golang.org/x/sys/unix.SyscallNoError",
		"s.go:4: uses golang.org/x/sys/unix.RawSyscallNoError",
		"s.go:5: uses golang.org/x/sys/unix.PtraceSetRegs",
		"s.go:6: uses golang.org/x/sys/unix.PivotRoot",
	})
}

// git.go runs git only as revParse's exec.LookPath("git") and
// exec.CommandContext(ctx, bin, argv...), bin the LookPath result; it
// names only the ExitError type besides.
func TestTheNetworkScanPinsTheGitProbe(t *testing.T) {
	allow := allowedFiles{listen: "listen.go", exec: map[string]execRule{"git.go": execRules[gitFile]}}
	files := writeFixtures(t, map[string]string{
		"git.go": "package x\nimport (\n\t\"context\"\n\t\"os/exec\"\n)\n" +
			"type first struct{}\n" +
			"func (first) revParse(ctx context.Context, argv []string) {\n\tbin := \"/bin/true\"\n\t_ = exec.CommandContext(ctx, bin, argv...)\n}\n" +
			"type prober struct{}\n" +
			"func (prober) revParse(dir string, args ...string) error {\n" +
			"\tbin, err := exec.LookPath(\"git\")\n" +
			"\tif err != nil {\n\t\treturn err\n\t}\n" +
			"\tctx, cancel := context.WithTimeout(context.Background(), 0)\n" +
			"\tdefer cancel()\n" +
			"\targv := append([]string{\"-C\", dir}, args...)\n" +
			"\tcmd := exec.CommandContext(ctx, bin, argv...)\n" +
			"\tcmd.Dir = dir\n" +
			"\tif _, ok := cmd.Run().(*exec.ExitError); ok {\n\t\treturn nil\n\t}\n" +
			"\treturn nil\n}\n" +
			"func (prober) other() {\n\t_ = exec.Command(\"/bin/true\")\n\t_, _ = exec.LookPath(\"git\")\n}\n",
	})
	got := scanNetworking(t, files, []typeUnit{fixtureUnit(files...)}, filepath.Base, allow)
	only := `only exec.LookPath("git") in revParse and exec.CommandContext(ctx, bin, argv...) in revParse may start a process`
	checkFindings(t, got.findings, []string{
		`git.go:9: exec.CommandContext(ctx, bin, argv...) in revParse: bin is not defined by exec.LookPath("git")`,
		"git.go:28: uses os/exec.Command: " + only,
		"git.go:29: uses os/exec.LookPath: " + only,
	})
	if want := []string{`revParse: exec.LookPath("git")`, "revParse: exec.CommandContext(ctx, bin, argv...)"}; !slices.Equal(got.commands["git.go"], want) {
		t.Errorf("commands = %v, want %v", got.commands["git.go"], want)
	}
}

// listPackages counts every kind of file the scan cannot read, and
// otherNative names the assembly and object files of other targets.
func TestListPackagesCountsNativeFiles(t *testing.T) {
	dir := t.TempDir()
	writeFixturesIn(t, dir, map[string]string{
		"go.mod":         "module example.com/native\n\ngo 1.26\n",
		"c.go":           "package native\n\n// int one(void);\nimport \"C\"\n",
		"one.c":          "int one(void) { return 1; }\n",
		"two.cc":         "int two() { return 2; }\n",
		"t.S":            "\n",
		"x.syso":         "",
		"w_windows.syso": "",
		"z_riscv64.s":    "\n",
	})
	pkgs := slices.DeleteFunc(listPackages(t, dir, ".", false, "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=1"),
		func(p listedPackage) bool { return p.Standard })
	if len(pkgs) != 1 {
		t.Fatalf("listed %+v", pkgs)
	}
	if got, want := nativeCode(pkgs[0]), "1 cgo, 1 C, 1 C++, 1 assembly, 1 syso"; got != want {
		t.Errorf("native code %q, want %q", got, want)
	}
	if got, want := otherNative(pkgs[0]), []string{"w_windows.syso", "z_riscv64.s"}; !slices.Equal(got, want) {
		t.Errorf("other targets' native files %q, want %q", got, want)
	}
}

// listTargets lists every scan target: a package linked only on darwin
// and one only on arm64 are covered, the other target's assembly and a
// process package linked on one target are problems, and each target's
// files are type-checked.
func TestTheScanListsEveryTarget(t *testing.T) {
	dir := t.TempDir()
	writeFixturesIn(t, dir, map[string]string{
		"go.mod":          "module example.com/targets\n\ngo 1.26\n",
		"main.go":         "package main\n\nfunc main() {}\n",
		"main_darwin.go":  "package main\n\nimport _ \"example.com/targets/mac\"\n",
		"main_arm64.go":   "package main\n\nimport _ \"example.com/targets/arm\"\n",
		"mac/mac.go":      "package mac\n\nimport _ \"plugin\"\n",
		"arm/arm.go":      "package arm\n",
		"arm/arm_ppc64.s": "\n",
		// Files no scan target builds are read by the name rules, and an
		// import only they add is not read at all: a finding.
		"main_linux_riscv64.go": "package main\n\nimport _ \"example.com/targets/riscv\"\n",
		"riscv/riscv.go":        "package riscv\n",
		"tagged.go":             "//go:build customtag\n\npackage main\n\nimport _ \"net/smtp\"\n",
	})
	l := listTargets(t, dir, ".", "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
	if got, want := slices.Sorted(maps.Keys(l.dirs)), []string{"example.com/targets", "example.com/targets/arm", "example.com/targets/mac"}; !slices.Equal(got, want) {
		t.Errorf("covered %v, want %v", got, want)
	}
	if got, want := slices.Sorted(slices.Values(l.problems)), []string{
		". links plugin on darwin/arm64, which can start a process or load code",
		"example.com/targets/arm has assembly or object files for other targets, which the scan cannot read: arm_ppc64.s",
	}; !slices.Equal(got, want) {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var checks []string
	for _, u := range l.units {
		checks = append(checks, u.target+" "+u.path)
	}
	want := []string{
		"linux/amd64 example.com/targets",
		"linux/arm64 example.com/targets/arm", "linux/arm64 example.com/targets",
		"darwin/arm64 example.com/targets/arm", "darwin/arm64 example.com/targets/mac", "darwin/arm64 example.com/targets",
	}
	if !slices.Equal(checks, want) {
		t.Errorf("type-checks %v, want %v", checks, want)
	}
	// Each target's files type-check, plugin and all; every Go file of
	// the covered packages is read, whatever its build constraints.
	display := func(f string) string { rel, _ := filepath.Rel(dir, f); return filepath.ToSlash(rel) }
	got := scanNetworking(t, scanFiles(t, l.dirs), l.units, display, allowedFiles{covered: l.covered()})
	checkFindings(t, got.findings, []string{
		"mac/mac.go:3: imports plugin, which can start a process or load code",
		"main_linux_riscv64.go:3: imports example.com/targets/riscv, which no scan target links, so the scan does not read it",
		"tagged.go:5: imports net/smtp",
	})
}
