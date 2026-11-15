package capture

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// CheckSelf refuses to capture when the server, described by its
// /proc/self/status, runs with euid 0, holds any effective or permitted
// capability, or runs under no_new_privs. Under root the same-uid target
// rule would cover every root daemon; under no_new_privs perf-agent's file
// capabilities are not granted at exec. Anything it cannot read exactly
// refuses too: a missing, repeated or malformed field, or a status the
// scanner cannot split.
func CheckSelf(status []byte) error {
	read := map[string]bool{"Uid": true, "CapEff": true, "CapPrm": true, "NoNewPrivs": true}
	fields := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || !read[k] {
			continue
		}
		if _, dup := fields[k]; dup {
			return Errorf(CodeDisabled, "", "/proc/self/status has %s twice", k)
		}
		fields[k] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return Errorf(CodeDisabled, "", "cannot read /proc/self/status: %v", err)
	}
	unreadable := func(field string) error {
		return Errorf(CodeDisabled, "", "cannot read %s from /proc/self/status", field)
	}

	// Uid is the real, effective, saved and filesystem uid, each a uid_t.
	uids := strings.Fields(fields["Uid"])
	if len(uids) != 4 {
		return unreadable("Uid")
	}
	var euid uint64
	for i, u := range uids {
		n, err := strconv.ParseUint(u, 10, 32)
		if err != nil {
			return unreadable("Uid")
		}
		if i == 1 {
			euid = n
		}
	}
	if euid == 0 {
		return Errorf(CodeDisabled, "run perf-agent-mcp as your own user; perf-agent gets its capabilities from setcap",
			"perf-agent-mcp runs with euid 0; capture is disabled, since the same-uid target rule would cover every root process")
	}
	for _, f := range []string{"CapEff", "CapPrm"} {
		v := fields[f]
		n, err := strconv.ParseUint(v, 16, 64)
		if err != nil {
			return unreadable(f)
		}
		if n != 0 {
			return Errorf(CodeDisabled, "run perf-agent-mcp without capabilities; only perf-agent is setcap'd",
				"perf-agent-mcp holds capabilities (%s %s: %s); the server must hold none",
				f, v, strings.Join(CapSet(n).Names(), ","))
		}
	}
	switch fields["NoNewPrivs"] {
	case "0":
	case "1":
		return Errorf(CodeDisabled, "start perf-agent-mcp outside the sandbox",
			"this server inherited no_new_privs (a sandbox?); perf-agent's file capabilities cannot apply")
	default:
		return unreadable("NoNewPrivs")
	}
	return nil
}
