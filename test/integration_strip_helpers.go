package test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stripWorkload copies src to dst, runs `objcopy --strip-all dst`, and
// returns dst's GNU build-id (lowercase hex). dst is the new path of
// the stripped binary; the original src is left untouched so other
// tests can reuse it.
func stripWorkload(t *testing.T, src, dst string) string {
	t.Helper()
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copy %s → %s: %v", src, dst, err)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", dst, err)
	}
	cmd := exec.Command("objcopy", "--strip-all", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("objcopy --strip-all %s: %v\n%s", dst, err, out)
	}
	return readBuildID(t, dst)
}

// uploadDebug invokes test/debuginfod/upload.sh to extract debug info
// from srcWithDwarf and deposit it under the debuginfod store at
// test/debuginfod/debuginfo-store. Returns the build-id and the
// expected store-relative path of the .debug file.
//
// The caller is responsible for waiting for the debuginfod server to
// re-scan — call waitForDebuginfodReady(t, buildID) before profiling.
func uploadDebug(t *testing.T, srcWithDwarf string) (buildID, debugPath string) {
	t.Helper()
	abs, err := filepath.Abs(srcWithDwarf)
	if err != nil {
		t.Fatalf("abspath %s: %v", srcWithDwarf, err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	fixtureDir := filepath.Join(wd, "debuginfod")
	storeDir := filepath.Join(fixtureDir, "debuginfo-store")
	uploadScript := filepath.Join(fixtureDir, "upload.sh")

	cmd := exec.Command(uploadScript, abs)
	cmd.Env = append(os.Environ(), "STORE="+storeDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("upload.sh %s: %v", abs, err)
	}

	buildID = readBuildID(t, srcWithDwarf)
	debugPath = filepath.Join("debuginfod", "debuginfo-store", ".build-id",
		buildID[:2], buildID[2:]+".debug")
	return buildID, debugPath
}

// readBuildID parses `readelf -n` output and returns the GNU build-id
// (lowercase hex). Fatals if the binary has no build-id.
func readBuildID(t *testing.T, path string) string {
	t.Helper()
	cmd := exec.Command("readelf", "-n", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("readelf -n %s: %v", path, err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Build ID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Build ID:"))
		}
	}
	t.Fatalf("no GNU Build ID in %s:\n%s", path, out)
	return ""
}

// waitForDebuginfodReady polls http://localhost:8002/buildid/<id>/debuginfo
// for up to 30s, returning when the server starts serving the build-id
// (HTTP 200) or fataling on timeout.
//
// The local debuginfod docker container rescans the bind-mounted store
// every ~10s; this helper bridges that gap.
func waitForDebuginfodReady(t *testing.T, buildID string) {
	t.Helper()
	url := fmt.Sprintf("http://localhost:8002/buildid/%s/debuginfo", buildID)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("curl", "-fsS", "-o", "/dev/null", url)
		if err := cmd.Run(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("debuginfod did not serve %s within 30s", buildID)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
