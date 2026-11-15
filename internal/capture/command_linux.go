package capture

import (
	"os"
	"strings"
)

const commandNext = "profile a program that is not setuid, setgid or setcap'd"

// CheckCommandFile refuses a workload program that is not a regular,
// executable file, or that is setuid, setgid or carries file
// capabilities. It follows symlinks, as the exec will. no_new_privs is
// the boundary that keeps such a program from gaining privilege; this
// check gives the agent a clear error first.
func CheckCommandFile(path string) *Error {
	fi, err := os.Stat(path)
	if err != nil {
		return Errorf(CodeInvalidArgument, "", "cannot stat command %s: %v", path, err)
	}
	raw, err := readCapXattr(path)
	if err != nil {
		return Errorf(CodeInvalidArgument, "", "cannot read the file capabilities of command %s: %v", path, err)
	}
	return checkCommandFile(path, fi, raw)
}

// checkCommandFile applies the command rules to a stat result and the raw
// security.capability value (nil when there is none).
func checkCommandFile(path string, fi os.FileInfo, xattr []byte) *Error {
	mode := fi.Mode()
	switch {
	case !mode.IsRegular():
		return Errorf(CodeInvalidArgument, "pass a program file", "command %s is not a regular file", path)
	case mode&os.ModeSetuid != 0:
		return Errorf(CodeInvalidArgument, commandNext, "command %s is setuid", path)
	case mode&os.ModeSetgid != 0:
		return Errorf(CodeInvalidArgument, commandNext, "command %s is setgid", path)
	case mode&0o111 == 0:
		return Errorf(CodeInvalidArgument, "chmod u+x "+path, "command %s is not executable", path)
	}
	if xattr == nil {
		return nil
	}
	if caps, err := ParseFileCaps(xattr); err == nil {
		return Errorf(CodeInvalidArgument, commandNext, "command %s has file capabilities (%s)", path, strings.Join(caps.Names(), ","))
	}
	return Errorf(CodeInvalidArgument, commandNext, "command %s has file capabilities (a %s value capture cannot parse)", path, capXattr)
}
