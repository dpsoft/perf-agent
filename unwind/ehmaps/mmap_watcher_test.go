package ehmaps

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMmapWatcherSeesMmap attaches a watcher to the test process itself
// and then does a deliberate executable file mmap, verifying the watcher
// picks up the event. Attaching to ourselves + controlling the mmap
// eliminates child-startup races.
//
// requireBPFCaps is defined in ehmaps_runtime_test.go; do not redefine.
func TestMmapWatcherSeesMmap(t *testing.T) {
	requireBPFCaps(t)

	// Pin this goroutine to the OS thread that already matches
	// os.Getpid() (the main TID). Without this, Go may run unix.Mmap
	// on a worker thread whose TID differs from os.Getpid(), and the
	// perf_event (opened against pid=os.Getpid()) never sees the mmap.
	// PerfBitInherit in the watcher covers worker threads too, but
	// locking here removes all ambiguity for this test.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w, err := NewMmapWatcher(uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("NewMmapWatcher: %v", err)
	}
	defer w.Close()

	// Let the reader goroutine start draining.
	time.Sleep(100 * time.Millisecond)

	const target = "/bin/ls"
	f, err := os.Open(target)
	if err != nil {
		t.Skipf("%s not available: %v", target, err)
	}
	defer f.Close()
	data, err := unix.Mmap(int(f.Fd()), 0, 4096, unix.PROT_READ|unix.PROT_EXEC, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap(%s, PROT_EXEC): %v", target, err)
	}
	defer unix.Munmap(data)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatal("event channel closed before /bin/ls MMAP2 observed")
			}
			if ev.Kind == MmapEvent && strings.HasSuffix(ev.Filename, "/ls") {
				return
			}
		case <-deadline:
			t.Fatal("no MMAP2 event for /bin/ls within 2s")
		}
	}
}
