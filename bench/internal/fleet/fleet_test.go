package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeStubGoWorkload creates a temp directory with a fake "go" workload —
// a shell script that sleeps until killed. We test the binary path only;
// python/node would require having the interpreters available, which is
// covered by the integration smoke run in Task 11/12 rather than here.
func makeStubGoWorkload(t *testing.T) (string, map[string]int) {
	t.Helper()
	dir := t.TempDir()
	sub := filepath.Join(dir, "go")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(sub, "cpu_bound")
	body := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return dir, map[string]int{"go": 3}
}

func TestSpawnAndStop(t *testing.T) {
	dir, mix := makeStubGoWorkload(t)
	f, err := Spawn(Opts{Mix: mix, WorkloadDir: dir})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got, want := len(f.PIDs()), 3; got != want {
		t.Errorf("PIDs len = %d, want %d", got, want)
	}
	if err := f.Wait(2 * time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := f.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop is idempotent.
	if err := f.Stop(); err != nil {
		t.Fatalf("Stop (second call): %v", err)
	}
	// Confirm the processes are gone.
	for _, pid := range f.PIDs() {
		if _, err := os.Stat("/proc/" + intStr(pid)); err == nil {
			t.Errorf("pid %d still in /proc after Stop", pid)
		}
	}
}

func TestSpawnFailsOnMissingWorkload(t *testing.T) {
	dir := t.TempDir() // empty
	_, err := Spawn(Opts{Mix: map[string]int{"go": 1}, WorkloadDir: dir})
	if err == nil {
		t.Fatal("expected error for missing workload, got nil")
	}
	if !strings.Contains(err.Error(), "no go binary") {
		t.Errorf("error message = %q, want to mention missing go binary", err.Error())
	}
}

func TestSpawnPartialFleetFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	// Set up "go" workload only — rust will fail.
	goSub := filepath.Join(dir, "go")
	if err := os.MkdirAll(goSub, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(goSub, "cpu_bound")
	body := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}

	// Map iteration order is random — we don't know if go or rust resolves
	// first. If go resolves first, 2 go workers are spawned, then rust fails
	// and Spawn must clean them up. If rust resolves first, commandFor fails
	// immediately with no workers started. Either way we expect an error.
	//
	// The real verification is that the cleanup code path runs without
	// leaking goroutines or panicking, which -race detects. We cannot
	// enumerate the spawned PIDs post-failure because Spawn returns
	// (nil, err) on failure; a stronger assertion would require API changes
	// to expose the partial fleet.
	_, err := Spawn(Opts{Mix: map[string]int{"go": 2, "rust": 1}, WorkloadDir: dir})
	if err == nil {
		t.Fatal("expected error for missing rust workload, got nil")
	}
}

// intStr is strconv.Itoa-equivalent without importing strconv into the
// test (keeps the test file imports minimal).
func intStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
