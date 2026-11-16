package capture

import (
	"io/fs"
	"os"
	"strings"
	"syscall"
)

const commandNext = "profile a program that is not setuid, setgid or setcap'd"

// CheckCommandFile refuses a workload program that is not a regular file
// this server's user may execute, or that is setuid, setgid or carries
// file capabilities. It follows symlinks, as the exec will. no_new_privs
// is the boundary that keeps such a program from gaining privilege; this
// check gives the agent a clear error first. path comes from the agent,
// so messages repeat it only cut.
func CheckCommandFile(path string) *Error {
	fi, err := os.Stat(path)
	if err != nil {
		return Errorf(CodeInvalidArgument, "", "cannot stat command %s: %v", echo(path), pathErr(err))
	}
	raw, err := readCapXattr(path)
	if err != nil {
		return Errorf(CodeInvalidArgument, "", "cannot read the file capabilities of command %s: %v", echo(path), pathErr(err))
	}
	return checkCommandFile(path, fi, raw)
}

// checkCommandFile applies the command rules to a stat result and the raw
// security.capability value (nil when there is none).
func checkCommandFile(path string, fi fs.FileInfo, xattr []byte) *Error {
	mode := fi.Mode()
	switch {
	case !mode.IsRegular():
		return Errorf(CodeInvalidArgument, "pass a program file", "command %s is not a regular file", echo(path))
	case mode&os.ModeSetuid != 0:
		return Errorf(CodeInvalidArgument, commandNext, "command %s is setuid", echo(path))
	case mode&os.ModeSetgid != 0:
		return Errorf(CodeInvalidArgument, commandNext, "command %s is setgid", echo(path))
	case mode&0o111 == 0:
		return Errorf(CodeInvalidArgument, "make it executable (chmod u+x)", "command %s is not executable", echo(path))
	case !commandExecutable(fi, currentCreds()):
		return Errorf(CodeInvalidArgument, "make it executable by your user (chmod u+x for a file you own)",
			"command %s is not executable by this server's user (mode %#o; the kernel uses the owner, group or other bits that match it)", echo(path), mode.Perm())
	}
	if xattr == nil {
		return nil
	}
	if caps, err := ParseFileCaps(xattr); err == nil {
		return Errorf(CodeInvalidArgument, commandNext, "command %s has file capabilities (%s)", echo(path), strings.Join(caps.Names(), ","))
	}
	return Errorf(CodeInvalidArgument, commandNext, "command %s has file capabilities (a %s value capture cannot parse)", echo(path), capXattr)
}

// commandExecutable reports whether c may execute the file fi describes,
// by the kernel's owner, group (effective or supplementary) or other
// class, as for perf-agent itself.
func commandExecutable(fi fs.FileInfo, c creds) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && executableBy(st, c)
}

// executableHere returns the check ResolveCommand applies to each PATH
// candidate, with this server's credentials read once.
func executableHere() func(fs.FileInfo) bool {
	c := currentCreds()
	return func(fi fs.FileInfo) bool { return commandExecutable(fi, c) }
}
