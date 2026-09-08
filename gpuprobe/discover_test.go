package gpuprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A fake /proc whose maps files can be written per pid, plus a real file on
// disk to stand in for the shim -- real, because identity comes from stat(2)
// and a fixture cannot fake an inode.
func fakeProcWithShim(t *testing.T) (procRoot, shimPath string, maj, min uint32, ino uint64) {
	t.Helper()
	dir := t.TempDir()
	shimPath = filepath.Join(dir, "libperfagent-gpu-nvidia.so")
	require.NoError(t, os.WriteFile(shimPath, []byte("\x7fELF not really"), 0o755))
	var st unix.Stat_t
	require.NoError(t, unix.Stat(shimPath, &st))
	return t.TempDir(), shimPath, unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino
}

func writeMaps(t *testing.T, procRoot string, pid int, lines string) {
	t.Helper()
	d := filepath.Join(procRoot, fmt.Sprint(pid))
	require.NoError(t, os.MkdirAll(d, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d, "maps"), []byte(lines), 0o600))
}

func mapsLine(maj, min uint32, ino uint64, path string) string {
	return fmt.Sprintf("7fc9a0a00000-7fc9a0a21000 r-xp 00001000 %02x:%02x %d %s\n", maj, min, ino, path)
}

// The whole point: find the targets without being told what they are.
func TestDiscoveryFindsExactlyTheProcessesMappingTheShim(t *testing.T) {
	procRoot, shim, maj, min, ino := fakeProcWithShim(t)

	writeMaps(t, procRoot, 101, mapsLine(maj, min, ino, shim))
	writeMaps(t, procRoot, 202, mapsLine(maj, min, ino+1, "/lib/libsomethingelse.so"))
	writeMaps(t, procRoot, 303, mapsLine(maj, min, ino, shim))
	// A non-pid entry: /proc is full of them (self, sys, meminfo) and a scan
	// that tried to read them all as pids would be noisy at best.
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "sys"), 0o755))

	got, err := processesMappingShimIn(procRoot, shim)
	require.NoError(t, err)
	assert.Equal(t, []int{101, 303}, got,
		"discovery must return every process mapping the shim inode and no others")
}

// A host runs other people's processes. If a single unreadable one aborted the
// scan, discovery would report no targets on any multi-tenant machine -- and
// "nothing is being profiled" is what that failure would look like.
func TestAnUnreadableProcessDoesNotHideTheReadableOnes(t *testing.T) {
	procRoot, shim, maj, min, ino := fakeProcWithShim(t)

	// 111 sorts first and cannot be read at all: no maps file, exactly as a
	// process that exited between readdir and open presents.
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "111"), 0o755))
	writeMaps(t, procRoot, 222, mapsLine(maj, min, ino, shim))

	got, err := processesMappingShimIn(procRoot, shim)
	require.NoError(t, err)
	assert.Equal(t, []int{222}, got,
		"an unreadable pid must be skipped, not abort the scan")
}

// Identity is the inode, not the path, because that is what a uprobe attaches
// to. A second copy of the shim is a second file: a process mapping it will
// never fire probes attached to the first, however identical the maps line
// reads. Getting this wrong would make a sidecar attach confidently to a pod
// running a different build and report nothing, forever, with no error.
func TestASecondCopyOfTheShimIsADifferentTarget(t *testing.T) {
	procRoot, shim, maj, min, ino := fakeProcWithShim(t)

	copyPath := filepath.Join(filepath.Dir(shim), "copy-of-shim.so")
	orig, err := os.ReadFile(shim)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(copyPath, orig, 0o755))
	var st unix.Stat_t
	require.NoError(t, unix.Stat(copyPath, &st))
	require.NotEqual(t, ino, st.Ino, "the fixture must actually be a second inode")

	// 404 maps the copy. Its maps line even NAMES a path ending in .so from
	// the same directory -- a path-matching implementation would take it.
	writeMaps(t, procRoot, 404, mapsLine(maj, min, st.Ino, copyPath))
	writeMaps(t, procRoot, 505, mapsLine(maj, min, ino, shim))

	got, err := processesMappingShimIn(procRoot, shim)
	require.NoError(t, err)
	assert.Equal(t, []int{505}, got,
		"byte-identical copies are different files to a uprobe and must be to discovery")

	got, err = processesMappingShimIn(procRoot, copyPath)
	require.NoError(t, err)
	assert.Equal(t, []int{404}, got, "and the reverse must hold, or the test proves nothing")
}

// No targets is an ANSWER, not an error. A collector that starts before any
// GPU pod is scheduled is in a normal state, and the caller decides what to do
// about it; a scan that errored could not tell that apart from a broken /proc.
func TestNoTargetsIsAnEmptyAnswerRatherThanAnError(t *testing.T) {
	procRoot, shim, maj, min, ino := fakeProcWithShim(t)
	writeMaps(t, procRoot, 999, mapsLine(maj, min, ino+7, "/lib/libc.so.6"))

	got, err := processesMappingShimIn(procRoot, shim)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// A shim path that cannot be stat'd is a different failure from one nothing
// maps, and callers route them to different fixes: the operator's command line
// versus how the target was started.
func TestAnUnidentifiableShimIsAnError(t *testing.T) {
	procRoot := t.TempDir()
	_, err := processesMappingShimIn(procRoot, filepath.Join(procRoot, "does-not-exist.so"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identify shim")
}

// The exported entry point reads the real /proc, and this process maps its own
// libraries -- so a library out of our own maps must find at least us. Guards
// against the whole thing being wired to a procRoot that does not exist.
func TestTheExportedScanReadsTheRealProc(t *testing.T) {
	self, err := os.Readlink("/proc/self/exe")
	require.NoError(t, err)
	got, err := ProcessesMappingShim(self)
	require.NoError(t, err)
	assert.Contains(t, got, os.Getpid(),
		"this process maps its own executable; the real /proc scan must find it")
}
