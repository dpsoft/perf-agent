package capture

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ResolveCommand returns the program a workload's command[0] names:
//   - an absolute name is used as is;
//   - a bare name is looked up once in pathEnv, the scrubbed PATH the
//     workload itself gets, taking the first absolute entry that holds a
//     regular, executable file of that name; relative and empty entries
//     are skipped, since they would depend on the server's directory;
//   - anything else (./x, bin/x) is invalid_argument.
//
// With allow set (--capture-command), the result must equal one of its
// entries exactly.
func ResolveCommand(name, pathEnv string, allow []string) (string, *Error) {
	const next = "pass command[0] as an absolute path or a bare name found in PATH"
	var resolved string
	switch {
	case name == "":
		return "", Errorf(CodeInvalidArgument, next, "the command is empty")
	case filepath.IsAbs(name):
		resolved = name
	case strings.ContainsRune(name, '/'):
		return "", Errorf(CodeInvalidArgument, next, "command[0] %q is a relative path", echo(name))
	default:
		for dir := range strings.SplitSeq(pathEnv, string(os.PathListSeparator)) {
			if !filepath.IsAbs(dir) {
				continue
			}
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
				resolved = p
				break
			}
		}
		if resolved == "" {
			return "", Errorf(CodeInvalidArgument, next, "command %q is not an executable file in the workload's PATH (%s)", echo(name), cut(pathEnv, maxPathEcho))
		}
	}
	if len(allow) > 0 && !slices.Contains(allow, resolved) {
		return "", Errorf(CodeInvalidArgument, "use one of the programs perf-agent-mcp was started with --capture-command",
			"%s is not one of the --capture-command programs", echo(resolved))
	}
	return resolved, nil
}

// maxEcho is the most characters of an agent's argument a message
// repeats, as in the MCP layer; maxPathEcho bounds a server path.
const (
	maxEcho     = 64
	maxPathEcho = 200
)

// echo is an agent's argument cut to maxEcho characters.
func echo(s string) string { return cut(s, maxEcho) }

// cut is s cut to n characters, ending in "…" when cut.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// WorkloadEnv returns the environment a workload starts with: from
// environ, in order, only PATH, HOME, USER, LOGNAME, LANG, LC_*, TZ,
// TMPDIR, XDG_RUNTIME_DIR and CUDA_VISIBLE_DEVICES, each once (the first
// wins, as getenv sees it), then CUDA_INJECTION64_PATH=shim for a gpu
// capture.
func WorkloadEnv(environ []string, shim string) []string {
	var env []string
	seen := map[string]bool{}
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || seen[k] || !workloadKey(k) {
			continue
		}
		seen[k] = true
		env = append(env, kv)
	}
	if shim != "" {
		env = append(env, "CUDA_INJECTION64_PATH="+shim)
	}
	return env
}

func workloadKey(k string) bool {
	switch k {
	case "PATH", "HOME", "USER", "LOGNAME", "LANG", "TZ", "TMPDIR", "XDG_RUNTIME_DIR", "CUDA_VISIBLE_DEVICES":
		return true
	}
	return strings.HasPrefix(k, "LC_")
}
