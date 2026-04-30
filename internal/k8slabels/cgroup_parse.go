package k8slabels

import (
	"path/filepath"
	"regexp"
	"strings"
)

// parseV2CgroupPath scans a /proc/<pid>/cgroup file body and returns the
// cgroup v2 path (the line beginning with "0::"). Hybrid hosts (cgroup v1
// + v2 mounted) include both formats; pure v1 hosts have no 0:: line.
func parseV2CgroupPath(body []byte) (string, bool) {
	for line := range strings.Lines(string(body)) {
		line = strings.TrimRight(line, "\r\n")
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return rest, true
		}
	}
	return "", false
}

// podUIDRE matches the pod-UID segment in a kubepods cgroup path. Two
// driver styles are produced by kubelet:
//
//   - cgroupfs driver: pod<UID> with all-dashes separators
//     (e.g. pod12345678-1234-1234-1234-123456789abc)
//   - systemd driver: ...pod<UID>.slice with all-underscores separators
//     (e.g. ...pod12345678_1234_1234_1234_123456789abc.slice)
//
// Each alternative requires homogeneous separators within a single UID;
// mixed separators are not produced by any kubelet and would indicate a
// malformed path. Canonicalisation to dashes happens after extraction.
var podUIDRE = regexp.MustCompile(
	`pod(` +
		`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}` +
		`|` +
		`[0-9a-fA-F]{8}_[0-9a-fA-F]{4}_[0-9a-fA-F]{4}_[0-9a-fA-F]{4}_[0-9a-fA-F]{12}` +
		`)`,
)

func extractPodUID(cgroupPath string) string {
	if !strings.Contains(cgroupPath, "kubepods") {
		return ""
	}
	m := podUIDRE.FindStringSubmatch(cgroupPath)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "_", "-")
}

// containerIDRuntimePrefixes is the set of leaf-segment prefixes used by
// supported container runtimes when running under k8s. Order matters only
// for documentation: each is checked exhaustively.
var containerIDRuntimePrefixes = []string{
	"cri-containerd-",
	"crio-",
	"docker-",
}

func extractContainerID(cgroupPath string) string {
	leaf := filepath.Base(cgroupPath)
	if leaf == "." || leaf == ".." || leaf == "/" {
		return ""
	}
	// Strip the .scope suffix (systemd driver) before checking prefixes.
	stripped := strings.TrimSuffix(leaf, ".scope")
	for _, prefix := range containerIDRuntimePrefixes {
		if rest, ok := strings.CutPrefix(stripped, prefix); ok {
			return rest
		}
	}
	// cgroupfs driver: leaf is the raw container ID. Heuristic: looks like
	// a hex blob (≥12 chars, all hex). Avoids matching "kubepods-burstable.slice".
	if len(stripped) >= 12 && isHex(stripped) {
		return stripped
	}
	return ""
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
