package gpuprobe

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/crc64"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"golang.org/x/sys/unix"
)

// The cubin channel is tested the way the rendezvous is: against a real
// abstract socket, a real memfd with real seals, and this test binary's own
// process as the peer. No GPU, no BPF, no capabilities.
//
// The trick is the same one enroll_test.go uses - point the listener at
// /proc/self/exe, which this process genuinely maps, so procMapsHaveInode
// passes for real rather than through a mock that would agree with anything.

// testCubinListener binds a cubin listener over `sink` and tears it down with
// the test, so a leaked goroutine or a leaked abstract address fails the next
// run rather than this one.
func testCubinListener(t *testing.T, cfg Config, sink cubinSink) *cubinListener {
	t.Helper()
	l, err := newCubinListener(cfg, sink)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.close() })
	return l
}

// sealedCubinFD is the producer's half of the payload: a memfd holding body,
// with exactly the seals in `seals` and no others.
//
// F_SEAL_SEAL is applied last on purpose - it is the seal that forbids adding
// more - so a subset that includes it still ends up with the rest.
func sealedCubinFD(t *testing.T, body []byte, seals int) int {
	t.Helper()
	fd, err := unix.MemfdCreate("perfagent-cubin-test", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	for off := 0; off < len(body); {
		n, werr := unix.Write(fd, body[off:])
		require.NoError(t, werr)
		off += n
	}
	for _, s := range []int{unix.F_SEAL_SHRINK, unix.F_SEAL_GROW, unix.F_SEAL_WRITE, unix.F_SEAL_SEAL} {
		if seals&s == 0 {
			continue
		}
		_, ferr := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, s)
		require.NoError(t, ferr, "F_ADD_SEALS %s", cubinSealNames(s))
	}
	return fd
}

// offerHeader is a well-formed header for `size` bytes under `crc`.
func offerHeader(size, crc uint64) cubinHeader {
	return cubinHeader{magic: cubinHeaderMagic, version: cubinHeaderVersion, size: size, crc: crc}
}

// offerCubin dials the offer channel the way the shim does - one sendmsg
// carrying the header and the descriptor - and returns the status byte, or 0
// when the connection closed without one.
func offerCubin(t *testing.T, addr string, h cubinHeader, fd int) byte {
	t.Helper()
	return offerCubinFDs(t, addr, encodeCubinHeader(h), fdList(fd))
}

func fdList(fd int) []int {
	if fd < 0 {
		return nil
	}
	return []int{fd}
}

func offerCubinFDs(t *testing.T, addr string, hdr []byte, fds []int) byte {
	t.Helper()
	conn, err := net.DialTimeout("unix", addr, 2*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	uc, ok := conn.(*net.UnixConn)
	require.True(t, ok)
	require.NoError(t, uc.SetDeadline(time.Now().Add(5*time.Second)))
	var rights []byte
	if len(fds) > 0 {
		rights = unix.UnixRights(fds...)
	}
	// EPIPE / ECONNRESET here is not a failure: the listener decides
	// unauthorized and throttled offers from the peer's credentials alone,
	// without ever reading, so it can have replied 'X' and closed before this
	// write lands. The status byte it wrote is still queued on this end and
	// the read below returns it. Failing on the write instead makes every
	// reject-before-read test a coin toss on how the two ends interleave -
	// reproducibly lost on 6.19, and the reason this is not a require.
	//
	// (Task 5 note: pre-existing. TestAPerPIDConsumerRefusesCubinsFromEveryOtherPID
	// fails identically on origin/feat/cubin-transport unmodified.)
	if _, _, werr := uc.WriteMsgUnix(hdr, rights, nil); werr != nil {
		require.True(t, errors.Is(werr, unix.EPIPE) || errors.Is(werr, unix.ECONNRESET),
			"unexpected write error: %v", werr)
	}
	var b [1]byte
	n, err := uc.Read(b[:])
	if err != nil || n != 1 {
		return 0
	}
	return b[0]
}

// recordingCubinSink is the store under test's downstream: it records exactly
// what was handed to it, so "nothing was stored" is an assertion rather than
// an assumption. `gate`, when set, wedges PutCubin so a test can hold the
// cubin listener's serial accept loop open.
type recordingCubinSink struct {
	mu      sync.Mutex
	have    map[uint64][]byte
	puts    int
	err     error
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once

	// resident models a store that EVICTS: it is what the sink says it holds,
	// which a test can hold flat while lifetime bytes climb. That divergence
	// is issue #96 and it cannot be shown with a sink whose two numbers are
	// always equal.
	resident int64
	evicting bool
}

func newRecordingCubinSink() *recordingCubinSink {
	return &recordingCubinSink{have: map[uint64][]byte{}, entered: make(chan struct{})}
}

func (s *recordingCubinSink) HasCubin(crc uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.have[crc]
	return ok
}

func (s *recordingCubinSink) PutCubin(crc uint64, b []byte) error {
	s.once.Do(func() { close(s.entered) })
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.err != nil {
		return s.err
	}
	s.have[crc] = b
	if !s.evicting {
		s.resident += int64(len(b))
	}
	return nil
}

// ResidentBytes is what the listener charges its total ceiling against.
func (s *recordingCubinSink) ResidentBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resident
}

func (s *recordingCubinSink) get(crc uint64) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.have[crc]
	return b, ok
}

func (s *recordingCubinSink) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func (s *recordingCubinSink) stored() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.have)
}

// cubinFixture is a deterministic stand-in for a real cubin: this task moves
// BYTES, and whether those bytes are an ELF is Task 1's question. A repeating
// non-uniform pattern so a truncation or an off-by-one page shows up as a
// content mismatch rather than as two equal runs of zeros.
func cubinFixture(n int) []byte {
	b := make([]byte, n)
	copy(b, "\x7fELF\x02\x01\x01\x00")
	for i := 8; i < n; i++ {
		b[i] = byte(i*31 + i/251)
	}
	return b
}

var cubinCRCTable = crc64.MakeTable(crc64.ECMA)

// assertNoCubinRejections is the "green run reads zero" assertion, spelled
// once. Ten defects on this project were counters reading green exactly when
// things were worst, so every rejection counter is checked at zero on a
// healthy path rather than only checked at one on a broken path.
func assertNoCubinRejections(t *testing.T, st cubinStats) {
	t.Helper()
	assert.Zero(t, st.tooLarge, "CubinsRejectedTooLarge on a healthy run")
	assert.Zero(t, st.malformed, "CubinsRejectedMalformed on a healthy run")
	assert.Zero(t, st.unsealed, "CubinsRejectedUnsealed on a healthy run")
	assert.Zero(t, st.unauthorized, "CubinsRejectedUnauthorized on a healthy run")
	assert.Zero(t, st.throttled, "CubinsThrottled on a healthy run")
}

// The address is a sibling of the rendezvous, not the same socket: the same
// dev:inode derivation - so it inherits the btrfs stat-versus-maps fix - under
// a different name, so it is a different listener with a different bucket.
func TestTheCubinAddressIsASiblingOfTheRendezvousAndNotTheSameSocket(t *testing.T) {
	// The source tree's filesystem, not TMPDIR: on tmpfs stat(2) and /proc
	// report the same device, and this assertion cannot then tell a
	// stat-derived address from a /proc-derived one. See repoTempDir.
	dir := repoTempDir(t)
	a := filepath.Join(dir, "shim-a")
	b := filepath.Join(dir, "shim-b")
	require.NoError(t, os.WriteFile(a, []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("b"), 0o600))

	cubinA, err := cubinAddress(a)
	require.NoError(t, err)
	enrollA, err := enrollAddress(a)
	require.NoError(t, err)

	assert.NotEqual(t, enrollA, cubinA,
		"the two channels share an address: an offer would then queue ahead of an enrolment and spend its admission token")
	assert.True(t, strings.HasPrefix(cubinA, "@perfagent-gpu-cubin.v1."), "got %q", cubinA)

	// One derivation, two prefixes. The tail is the dev:inode the KERNEL
	// reports, which is the whole btrfs fix, so it must be character-for-
	// character the same on both channels rather than merely equal today.
	assert.Equal(t,
		strings.TrimPrefix(enrollA, "@perfagent-gpu-enroll.v1."),
		strings.TrimPrefix(cubinA, "@perfagent-gpu-cubin.v1."),
		"the two channels derived different dev:inode tails for the same file")

	// And it is pinned against /proc, not stat(2), exactly as the enrolment
	// address is - the mistake that bound a name no producer ever dialled.
	wantDev, wantIno := mapsIdentityOf(t, a)
	assert.Equal(t,
		"@perfagent-gpu-cubin.v1."+itoa(uint64(unix.Major(wantDev)))+"."+
			itoa(uint64(unix.Minor(wantDev)))+"."+itoa(wantIno), cubinA)

	cubinB, err := cubinAddress(b)
	require.NoError(t, err)
	assert.NotEqual(t, cubinA, cubinB,
		"two copies of a shim must not share an offer channel")

	_, err = cubinAddress(filepath.Join(dir, "not-there"))
	assert.Error(t, err)
}

// The transport's job, stated plainly: the bytes that went in come out, at
// both ends of the plausible size range. 2 MB is the plan's own upper figure
// for a cubin, and it is far past one socket buffer - which is exactly why
// the payload is a mapped memfd and not a stream.
func TestACubinArrivesByteIdenticalAtFiveKilobytesAndAtTwoMegabytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"5 KB", 5 * 1024},
		{"2 MB", 2 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim := selfExe(t)
			sink := newRecordingCubinSink()
			l := testCubinListener(t, Config{ShimPath: shim}, sink)

			body := cubinFixture(tc.size)
			crc := crc64.Checksum(body, cubinCRCTable)
			fd := sealedCubinFD(t, body, cubinRequiredSeals)

			require.Equal(t, byte(cubinReplyOK), offerCubin(t, l.address(), offerHeader(uint64(len(body)), crc), fd))

			got, ok := sink.get(crc)
			require.True(t, ok, "the offer was accepted and nothing was stored")
			assert.Equal(t, len(body), len(got))
			assert.True(t, bytes.Equal(body, got),
				"the bytes changed in transit; a wrong cubin parses into a wrong line table")
			assert.Equal(t, crc, crc64.Checksum(got, cubinCRCTable),
				"the stored bytes do not hash to the CRC the fixture was keyed by")

			st := l.snapshot()
			assert.Equal(t, uint64(1), st.received)
			assert.Equal(t, uint64(len(body)), st.bytes)
			assert.Equal(t, uint64(1), st.mapped, "the payload must be mapped, not streamed")
			assertNoCubinRejections(t, st)
		})
	}
}

// An oversized cubin is rejected WHOLE. Never truncated to fit: a truncated
// cubin parses into a wrong line table, which is the one failure worse than
// no line table.
func TestAnOfferOverThePerCubinCeilingIsRejectedWholeAndCounted(t *testing.T) {
	shim := selfExe(t)
	sink := newRecordingCubinSink()
	const ceiling = 64 * 1024
	l := testCubinListener(t, Config{ShimPath: shim, CubinMaxBytes: ceiling}, sink)

	// Exactly one byte over.
	body := cubinFixture(ceiling + 1)
	fd := sealedCubinFD(t, body, cubinRequiredSeals)
	require.Equal(t, byte(cubinReplyRefused), offerCubin(t, l.address(), offerHeader(uint64(len(body)), 1), fd))

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.tooLarge)
	assert.Zero(t, st.received)
	assert.Zero(t, st.bytes)
	assert.Zero(t, st.mapped, "an over-ceiling offer was mapped anyway")
	assert.Zero(t, sink.putCount(), "nothing may be stored for a rejected offer, whole or partial")
	assert.Contains(t, st.lastErr, "per-cubin ceiling")

	// And the ceiling itself is inclusive: exactly at it is fine, so the
	// rejection above is one byte of policy rather than an off-by-one.
	ok := cubinFixture(ceiling)
	okCRC := crc64.Checksum(ok, cubinCRCTable)
	okFD := sealedCubinFD(t, ok, cubinRequiredSeals)
	require.Equal(t, byte(cubinReplyOK), offerCubin(t, l.address(), offerHeader(uint64(len(ok)), okCRC), okFD))
	assert.Equal(t, uint64(1), l.snapshot().received)
}

// The second ceiling: everything this consumer will ever hold. Counted when
// it bites, and again nothing partial is stored.
func TestTheTotalBytesCeilingIsEnforcedAndCounted(t *testing.T) {
	shim := selfExe(t)
	sink := newRecordingCubinSink()
	const each = 32 * 1024
	l := testCubinListener(t, Config{ShimPath: shim, CubinTotalBytes: 3 * each}, sink)

	for i := range 3 {
		body := cubinFixture(each)
		body[8] = byte(i) // a distinct CRC per offer; duplicates are a no-op
		crc := crc64.Checksum(body, cubinCRCTable)
		fd := sealedCubinFD(t, body, cubinRequiredSeals)
		require.Equal(t, byte(cubinReplyOK), offerCubin(t, l.address(), offerHeader(each, crc), fd),
			"offer %d is inside the total ceiling", i)
	}
	require.Equal(t, uint64(3*each), l.snapshot().bytes)

	over := cubinFixture(each)
	over[8] = 0xFF
	overCRC := crc64.Checksum(over, cubinCRCTable)
	overFD := sealedCubinFD(t, over, cubinRequiredSeals)
	assert.Equal(t, byte(cubinReplyRefused), offerCubin(t, l.address(), offerHeader(each, overCRC), overFD))

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.tooLarge)
	assert.Equal(t, uint64(3), st.received, "the fourth offer was stored past the total ceiling")
	assert.Equal(t, uint64(3*each), st.bytes)
	assert.Equal(t, uint64(3), st.mapped, "the refused offer was mapped anyway")
	assert.Equal(t, 3, sink.stored())
	assert.Contains(t, st.lastErr, "total ceiling")
}

// The header is a claim; the memfd is the fact. When they disagree the offer
// is refused, because a payload shorter than its declared size IS a truncated
// cubin and a longer one means the two ends disagree about what was offered.
func TestADeclaredSizeThatDisagreesWithThePayloadIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name          string
		actual        int
		declaredDelta int64
	}{
		{"declared larger than the payload", 4096, +1},
		{"declared smaller than the payload", 4096, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim := selfExe(t)
			sink := newRecordingCubinSink()
			l := testCubinListener(t, Config{ShimPath: shim}, sink)

			body := cubinFixture(tc.actual)
			fd := sealedCubinFD(t, body, cubinRequiredSeals)
			declared := uint64(int64(len(body)) + tc.declaredDelta)

			require.Equal(t, byte(cubinReplyRefused), offerCubin(t, l.address(), offerHeader(declared, 42), fd))

			st := l.snapshot()
			assert.Equal(t, uint64(1), st.malformed)
			assert.Zero(t, st.received)
			assert.Zero(t, st.mapped, "a size-mismatched offer was mapped anyway")
			assert.Zero(t, sink.putCount(), "no partial cubin may be stored")
			assert.Contains(t, st.lastErr, "memfd holds")
		})
	}
}

// The seals, one missing at a time. Each is a counted rejection and NONE of
// them is ever mapped - which is the entire property, because the two things
// the seals prevent (a peer ftruncating under our mmap and SIGBUSing us, and
// the ELF mutating under the parser) both happen at or after the map.
//
// There is deliberately no fallback branch that reads an unsealed offer
// anyway. Falling back is how a defended path becomes an undefended one.
func TestEachRequiredSealMissingInTurnIsRejectedAndNeverMapped(t *testing.T) {
	for _, tc := range []struct {
		missing int
		why     string
	}{
		{unix.F_SEAL_SHRINK, "a peer can ftruncate under our mmap and SIGBUS this process"},
		{unix.F_SEAL_WRITE, "the ELF mutates under the parser mid-parse"},
		{unix.F_SEAL_GROW, "the size we validated is not the size we map"},
		{unix.F_SEAL_SEAL, "the other three can be removed again"},
	} {
		t.Run(cubinSealNames(tc.missing), func(t *testing.T) {
			shim := selfExe(t)
			sink := newRecordingCubinSink()
			l := testCubinListener(t, Config{ShimPath: shim}, sink)

			body := cubinFixture(2048)
			crc := crc64.Checksum(body, cubinCRCTable)
			fd := sealedCubinFD(t, body, cubinRequiredSeals&^tc.missing)

			require.Equal(t, byte(cubinReplyRefused), offerCubin(t, l.address(), offerHeader(uint64(len(body)), crc), fd),
				"an offer missing %s was accepted; without it %s", cubinSealNames(tc.missing), tc.why)

			st := l.snapshot()
			assert.Equal(t, uint64(1), st.unsealed)
			assert.Zero(t, st.received)
			assert.Zero(t, st.mapped, "the agent mapped an unsealed memfd: %s", tc.why)
			assert.Zero(t, sink.putCount())
			assert.Contains(t, st.lastErr, cubinSealNames(tc.missing))
		})
	}

	// The all-seals case, so the four subtests above are known to be
	// rejecting the missing seal rather than rejecting everything.
	t.Run("all four present", func(t *testing.T) {
		shim := selfExe(t)
		sink := newRecordingCubinSink()
		l := testCubinListener(t, Config{ShimPath: shim}, sink)
		body := cubinFixture(2048)
		crc := crc64.Checksum(body, cubinCRCTable)
		fd := sealedCubinFD(t, body, cubinRequiredSeals)
		require.Equal(t, byte(cubinReplyOK), offerCubin(t, l.address(), offerHeader(uint64(len(body)), crc), fd))
		assertNoCubinRejections(t, l.snapshot())
	})
}

// A descriptor the peer did not seal is refused by the same check, whether it
// is unsealable outright or merely unsealed. Both land on CubinsRejectedUnsealed
// and neither is ever mapped.
//
// Worth pinning separately from the missing-seal table above, because the two
// arrive by different kernel paths: F_GET_SEALS returns EINVAL on a pipe,
// while a file on tmpfs - which IS shmem, and so IS sealable - answers with
// F_SEAL_SEAL alone and no write protection at all. The second is the
// dangerous one: it looks like a sealed object and the peer can still write
// through it.
func TestADescriptorThatIsNotASealedMemfdIsRefused(t *testing.T) {
	t.Run("a pipe, which cannot be sealed at all", func(t *testing.T) {
		shim := selfExe(t)
		sink := newRecordingCubinSink()
		l := testCubinListener(t, Config{ShimPath: shim}, sink)

		var fds [2]int
		require.NoError(t, unix.Pipe(fds[:]))
		t.Cleanup(func() { _ = unix.Close(fds[0]); _ = unix.Close(fds[1]) })

		require.Equal(t, byte(cubinReplyRefused),
			offerCubin(t, l.address(), offerHeader(1024, 9), fds[0]))

		st := l.snapshot()
		assert.Equal(t, uint64(1), st.unsealed)
		assert.Zero(t, st.mapped)
		assert.Zero(t, sink.putCount())
		assert.Contains(t, st.lastErr, "F_GET_SEALS")
	})

	t.Run("a tmpfs file, which is sealable but not sealed", func(t *testing.T) {
		shim := selfExe(t)
		sink := newRecordingCubinSink()
		l := testCubinListener(t, Config{ShimPath: shim}, sink)

		body := cubinFixture(1024)
		path := filepath.Join(t.TempDir(), "not-a-memfd")
		require.NoError(t, os.WriteFile(path, body, 0o600))
		f, err := os.Open(path)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()

		require.Equal(t, byte(cubinReplyRefused),
			offerCubin(t, l.address(), offerHeader(uint64(len(body)), 9), int(f.Fd())))

		st := l.snapshot()
		assert.Equal(t, uint64(1), st.unsealed)
		assert.Zero(t, st.mapped, "a writable file was mapped as if it were a sealed cubin")
		assert.Zero(t, sink.putCount())
		assert.Contains(t, st.lastErr, "F_SEAL_WRITE")
	})
}

// Every structural way a header can be wrong, all landing on one counter and
// none of them on a guess. A flag this consumer does not understand is a
// producer telling it something about the payload, and guessing is how a
// wrong line table gets built.
func TestAMalformedOfferIsRefusedAndCounted(t *testing.T) {
	body := cubinFixture(1024)
	for _, tc := range []struct {
		name string
		hdr  []byte
		fds  int
		want string
	}{
		{name: "bad magic", hdr: func() []byte {
			h := offerHeader(uint64(len(body)), 1)
			h.magic ^= 0xFF
			return encodeCubinHeader(h)
		}(), fds: 1, want: "bad magic"},
		{name: "unknown version", hdr: func() []byte {
			h := offerHeader(uint64(len(body)), 1)
			h.version = 7
			return encodeCubinHeader(h)
		}(), fds: 1, want: "unknown offer version"},
		{name: "reserved flag set", hdr: func() []byte {
			h := offerHeader(uint64(len(body)), 1)
			h.flags = 1
			return encodeCubinHeader(h)
		}(), fds: 1, want: "unknown offer flags"},
		{name: "zero declared size", hdr: encodeCubinHeader(offerHeader(0, 1)), fds: 1, want: "declared size is zero"},
		{name: "short header", hdr: encodeCubinHeader(offerHeader(uint64(len(body)), 1))[:8], fds: 1, want: "read offer header"},
		{name: "no descriptor", hdr: encodeCubinHeader(offerHeader(uint64(len(body)), 1)), fds: 0, want: "0 descriptors"},
		{name: "two descriptors", hdr: encodeCubinHeader(offerHeader(uint64(len(body)), 1)), fds: 2, want: "2 descriptors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim := selfExe(t)
			sink := newRecordingCubinSink()
			l := testCubinListener(t, Config{ShimPath: shim}, sink)

			var fds []int
			for range tc.fds {
				fds = append(fds, sealedCubinFD(t, body, cubinRequiredSeals))
			}
			assert.Equal(t, byte(cubinReplyRefused), offerCubinFDs(t, l.address(), tc.hdr, fds))

			st := l.snapshot()
			assert.Equal(t, uint64(1), st.malformed)
			assert.Zero(t, st.received)
			assert.Zero(t, st.mapped)
			assert.Zero(t, sink.putCount())
			assert.Contains(t, st.lastErr, tc.want)
		})
	}
}

// The same identity rule the rendezvous uses, reused verbatim: a peer that
// does not map the shim this consumer attached to is refused. Without it, any
// local process could feed a root profiler cubins for a module it never
// loaded - and a bogus cubin under a real CRC is a WRONG source line on every
// PC sample that joins to it, which is worse than none.
func TestACubinPeerThatDoesNotMapTheShimIsRefused(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "some-other-shim")
	require.NoError(t, os.WriteFile(shim, []byte("not mapped by anyone"), 0o600))

	sink := newRecordingCubinSink()
	l := testCubinListener(t, Config{ShimPath: shim}, sink)

	body := cubinFixture(1024)
	fd := sealedCubinFD(t, body, cubinRequiredSeals)
	assert.Equal(t, byte(cubinReplyRefused), offerCubin(t, l.address(), offerHeader(uint64(len(body)), 3), fd))

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.unauthorized)
	assert.Zero(t, st.received)
	assert.Zero(t, st.mapped, "an unauthorized peer's memfd was mapped")
	assert.Zero(t, sink.putCount())
	assert.Contains(t, st.lastErr, "does not map the shim")
}

// A per-PID attach must not accept another process's modules: its uprobes are
// filtered to one process and the socket name is not, so the listener has to
// be. Same rule as the rendezvous, same counter shape.
func TestAPerPIDConsumerRefusesCubinsFromEveryOtherPID(t *testing.T) {
	shim := selfExe(t)
	sink := newRecordingCubinSink()
	l := testCubinListener(t, Config{ShimPath: shim, PID: os.Getpid() + 1}, sink)

	body := cubinFixture(512)
	fd := sealedCubinFD(t, body, cubinRequiredSeals)
	assert.Equal(t, byte(cubinReplyRefused), offerCubin(t, l.address(), offerHeader(uint64(len(body)), 4), fd))

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.unauthorized)
	assert.Zero(t, st.mapped)
	assert.Contains(t, st.lastErr, "is not the attached pid")
}

// A CRC already held is a no-op decided from the header alone: cubin_crc is
// content-addressed, so the same CRC is the same bytes, and re-reading them
// could only reach the same answer at the cost of an mmap and a re-parse.
func TestAnOfferForAnAlreadyStoredCRCIsACountedNoOp(t *testing.T) {
	shim := selfExe(t)
	sink := newRecordingCubinSink()
	l := testCubinListener(t, Config{ShimPath: shim}, sink)

	body := cubinFixture(4096)
	crc := crc64.Checksum(body, cubinCRCTable)

	first := sealedCubinFD(t, body, cubinRequiredSeals)
	require.Equal(t, byte(cubinReplyOK), offerCubin(t, l.address(), offerHeader(uint64(len(body)), crc), first))

	second := sealedCubinFD(t, body, cubinRequiredSeals)
	require.Equal(t, byte(cubinReplyOK), offerCubin(t, l.address(), offerHeader(uint64(len(body)), crc), second),
		"a duplicate is a no-op, not a refusal: the producer has nothing to fix")

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.received, "the duplicate was stored a second time")
	assert.Equal(t, uint64(1), st.duplicate)
	assert.Equal(t, uint64(len(body)), st.bytes, "the duplicate was charged against the total ceiling")
	assert.Equal(t, uint64(1), st.mapped, "the duplicate was mapped: the CRC check runs before the payload is touched")
	assert.Equal(t, 1, sink.putCount())
	assertNoCubinRejections(t, st)
}

// A store that refuses is loss like any other, and it is counted rather than
// answered with an OK the producer would believe.
func TestAStoreFailureIsCountedAndTheOfferIsRefused(t *testing.T) {
	shim := selfExe(t)
	sink := newRecordingCubinSink()
	sink.err = fmt.Errorf("store is full")
	l := testCubinListener(t, Config{ShimPath: shim}, sink)

	body := cubinFixture(1024)
	fd := sealedCubinFD(t, body, cubinRequiredSeals)
	assert.Equal(t, byte(cubinReplyRefused), offerCubin(t, l.address(), offerHeader(uint64(len(body)), 11), fd))

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.malformed)
	assert.Zero(t, st.received)
	assert.Zero(t, st.bytes, "a cubin that was not stored must not be charged against the total ceiling")
	assert.Contains(t, st.lastErr, "store is full")
}

// The cubin bucket is a DIFFERENT bucket, with different numbers, and
// spending one cannot spend the other. This is the unit-level statement of
// the isolation the socket test below proves end to end.
func TestTheCubinAdmissionBucketIsItsOwn(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	cu := newCubinAdmission(clock)
	en := newEnrollAdmission(clock)

	admitted := 0
	for range cubinUIDBurst * 4 {
		if cu.admit(1000) {
			admitted++
		}
	}
	assert.Equal(t, cubinUIDBurst, admitted, "one uid spent more than the cubin burst with no time passing")

	// The enrolment bucket is untouched by all of that. Two objects, so this
	// is true by construction - which is the point of asserting it: the test
	// documents that nothing shares state, rather than measuring how much
	// leaks.
	for range enrollUIDBurst {
		require.True(t, en.admit(1000),
			"draining the cubin bucket cost an enrolment token; the buckets are not separate")
	}

	// And the cubin bucket refills on its own schedule.
	now = now.Add(time.Second)
	refilled := 0
	for range cubinUIDBurst * 2 {
		if cu.admit(1000) {
			refilled++
		}
	}
	assert.Equal(t, cubinUIDBurst, refilled)

	assert.Greater(t, cubinUIDBurst, enrollUIDBurst,
		"the cubin burst must exceed the enrolment burst: module loads cluster and enrolments do not")
}

// A throttled offer is refused instantly, told so, and costs no /proc read,
// no map and no store insert.
func TestAThrottledCubinOfferIsReleasedAndCountedNotServed(t *testing.T) {
	shim := selfExe(t)
	sink := newRecordingCubinSink()
	// Built but not yet serving, so the bucket can be drained before any peer
	// can reach it: `admit` has no lock precisely because only serve()
	// touches it.
	l, err := buildCubinListener(Config{ShimPath: shim}, sink)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.close() })
	for l.admit.admit(uint32(os.Getuid())) { //nolint:revive // drain until empty
	}
	l.start()

	body := cubinFixture(1024)
	fd := sealedCubinFD(t, body, cubinRequiredSeals)
	assert.Equal(t, byte(cubinReplyRefused), offerCubin(t, l.address(), offerHeader(uint64(len(body)), 5), fd))

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.throttled)
	assert.Zero(t, st.received)
	assert.Zero(t, st.mapped)
	assert.Zero(t, st.unauthorized, "throttling is its own outcome, not an identity refusal")
	assert.Zero(t, sink.putCount())
	assert.Contains(t, st.lastErr, "throttled")
}

// floodCubinOffers fires n WELL-FORMED offers at addr, concurrently, and
// returns a WaitGroup for them.
//
// Well-formed on purpose. A flood of connections that send nothing would
// exercise the header timeout rather than the admission bucket, and would
// prove nothing about throttling; these are the traffic a module-heavy
// workload actually generates. One shared sealed descriptor carries them all
// - the payload is identical, only the CRC differs - so the flood costs one
// memfd rather than n of them.
func floodCubinOffers(addr string, n, fd int, size, crcBase uint64) *sync.WaitGroup {
	rights := unix.UnixRights(fd)
	var wg sync.WaitGroup
	for i := range n {
		hdr := encodeCubinHeader(offerHeader(size, crcBase+uint64(i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("unix", addr, 20*time.Second)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			uc, ok := c.(*net.UnixConn)
			if !ok {
				return
			}
			_ = uc.SetDeadline(time.Now().Add(20 * time.Second))
			if _, _, werr := uc.WriteMsgUnix(hdr, rights, nil); werr != nil {
				return
			}
			var b [1]byte
			_, _ = uc.Read(b[:])
		}()
	}
	return &wg
}

// THE TEST THIS TASK EXISTS FOR.
//
// Flood the cubin channel until CubinsThrottled is non-zero, then perform a
// normal enrolment and assert it succeeds with UnwindEnrollThrottled
// unchanged - in BOTH orders.
//
// The "ahead" order is the one that matters and the one a shared socket
// fails. On a shared socket the enrolment's connect() lands in a backlog
// already holding a queue of offers, and the serial accept loop serves them
// first: the producer waits out its 2s budget and takes the lazy path, which
// is issue #49's ~38% stack loss. It also spends the same admission bucket,
// so past 32 offers in a second the enrolment is refused outright with only
// UnwindEnrollThrottled moving. Neither can happen here, and not because this
// test checks for it - because the offers are queued on a listener with its
// own Accept loop, its own goroutine and its own bucket. The test states the
// property; the separation is what makes it true.
//
// The "behind" order is the easy direction and is included so the pair is
// symmetric rather than selective.
func TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment(t *testing.T) {
	// Comfortably past both cubin buckets - the per-uid burst is the binding
	// one - with enough margin that a second's worth of refill during the
	// flood cannot absorb the overflow.
	const flood = cubinUIDBurst*2 + cubinTotalBurst

	t.Run("offers queued AHEAD of the enrolment", func(t *testing.T) {
		shim := selfExe(t)
		fake := newFakeRegistrar()
		el, _ := testEnrollListener(t, shim, 0, fake)

		sink := newRecordingCubinSink()
		sink.gate = make(chan struct{})
		cl := testCubinListener(t, Config{ShimPath: shim}, sink)

		body := cubinFixture(512)
		fd := sealedCubinFD(t, body, cubinRequiredSeals)

		// One genuine offer, wedged inside the store, so the cubin
		// listener's serial accept loop is occupied and everything below
		// queues in its backlog rather than being served. This is exactly
		// the shape that starves a shared Accept loop.
		go func() { _ = offerCubin(t, cl.address(), offerHeader(uint64(len(body)), 1), fd) }()
		select {
		case <-sink.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the wedging offer never reached the store")
		}

		// The flood, all of it now standing in front of the enrolment in
		// wall-clock order and none of it served.
		wg := floodCubinOffers(cl.address(), flood, fd, uint64(len(body)), 1000)

		// The enrolment, with the cubin channel wedged and its backlog full.
		// On a shared socket this is where the producer waits out its budget
		// and comes back kEnrollError.
		before := el.snapshot().throttled
		done := make(chan byte, 1)
		go func() { done <- dialEnroll(t, shim) }()
		select {
		case b := <-done:
			assert.Equal(t, byte(enrollReplyOK), b,
				"an enrolment was refused while the cubin channel was saturated")
		case <-time.After(5 * time.Second):
			t.Fatal("the enrolment did not complete while cubin offers were queued ahead of it; " +
				"the two channels are sharing an Accept loop")
		}
		assert.Equal(t, before, el.snapshot().throttled,
			"a cubin offer spent an enrolment admission token")
		assert.Equal(t, uint64(1), el.snapshot().confirmed)

		close(sink.gate)
		wg.Wait()

		cu := cl.snapshot()
		en := el.snapshot()
		t.Logf("cubin: offered=%d received=%d duplicate=%d throttled=%d | enrol: requests=%d confirmed=%d throttled=%d",
			flood+1, cu.received, cu.duplicate, cu.throttled, en.requests, en.confirmed, en.throttled)
		assert.Positive(t, cu.throttled,
			"the flood never exhausted the cubin bucket, so this proved nothing")
		assert.Positive(t, cu.received, "not one offer of the flood was actually served")
		assert.Zero(t, el.snapshot().throttled,
			"UnwindEnrollThrottled moved while only cubin offers were being refused")
	})

	t.Run("offers queued BEHIND the enrolment", func(t *testing.T) {
		shim := selfExe(t)
		fake := newFakeRegistrar()
		el, _ := testEnrollListener(t, shim, 0, fake)
		sink := newRecordingCubinSink()
		cl := testCubinListener(t, Config{ShimPath: shim}, sink)

		require.Equal(t, byte(enrollReplyOK), dialEnroll(t, shim))

		body := cubinFixture(512)
		fd := sealedCubinFD(t, body, cubinRequiredSeals)
		floodCubinOffers(cl.address(), flood, fd, uint64(len(body)), 2000).Wait()
		require.Positive(t, cl.snapshot().throttled, "the flood never exhausted the cubin bucket")
		cu := cl.snapshot()

		// A second enrolment, after the channel next door has been refusing
		// for a while, still lands.
		assert.Equal(t, byte(enrollReplyOK), dialEnroll(t, shim))
		st := el.snapshot()
		t.Logf("cubin: offered=%d received=%d duplicate=%d throttled=%d | enrol: requests=%d confirmed=%d throttled=%d",
			flood, cu.received, cu.duplicate, cu.throttled, st.requests, st.confirmed, st.throttled)
		assert.Zero(t, st.throttled, "UnwindEnrollThrottled moved because of cubin traffic")
		assert.Equal(t, uint64(2), st.confirmed)
	})
}

// The regression that keeps the two channels apart forever: the enrolment
// handler must NEVER read from its connection.
//
// shim/core/enroll.h states the protocol as "The producer sends nothing", and
// the producer implements exactly that - it connects and then blocks reading
// one byte. A read on this end would therefore block until the producer's own
// 2s budget expired and it closed, turning every rendezvous into a 2s stall
// ending in kEnrollError. That is the failure a shared socket would force,
// because discriminating an offer from an enrolment requires a read.
//
// Asserted twice, because either half alone is escapable: behaviourally, a
// producer that writes nothing is still answered promptly; and structurally,
// over the AST of handle() itself, so that adding the read later fails here
// rather than on a GPU three weeks afterwards.
func TestAnEnrolmentCompletesWithNoReadOnThatConnection(t *testing.T) {
	shim := selfExe(t)
	fake := newFakeRegistrar()
	l, _ := testEnrollListener(t, shim, 0, fake)

	conn, err := net.DialTimeout("unix", mustEnrollAddress(t, shim), 2*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	// Nothing is written. Ever. If the handler waits for a header this read
	// does not return until the deadline.
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	var b [1]byte
	start := time.Now()
	n, err := conn.Read(b[:])
	require.NoError(t, err, "the rendezvous did not answer a producer that sent nothing, "+
		"which is the whole protocol; a discriminating read has been added to handle()")
	require.Equal(t, 1, n)
	assert.Equal(t, byte(enrollReplyOK), b[0])
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Equal(t, uint64(1), l.snapshot().confirmed)

	assertNoReadInEnrollHandler(t)
}

func mustEnrollAddress(t *testing.T, shim string) string {
	t.Helper()
	addr, err := enrollAddress(shim)
	require.NoError(t, err)
	return addr
}

// assertNoReadInEnrollHandler walks the AST of enrollListener.handle and
// fails if it contains anything that reads from the connection.
//
// A behavioural test alone would pass for a handler that reads with a short
// deadline and falls through - which would still cost every producer that
// deadline, and would still be the shared-socket design creeping back in one
// commit at a time. This makes the rule structural.
func assertNoReadInEnrollHandler(t *testing.T) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "enroll.go", nil, 0)
	require.NoError(t, err)

	banned := map[string]bool{
		"Read": true, "ReadMsgUnix": true, "ReadFrom": true, "ReadFull": true,
		"ReadAll": true, "ReadByte": true, "SetReadDeadline": true, "NewScanner": true,
		"NewReader": true, "Recvmsg": true,
	}
	var handle *ast.FuncDecl
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "handle" || fn.Recv == nil {
			continue
		}
		handle = fn
	}
	require.NotNil(t, handle, "enrollListener.handle not found in enroll.go")

	ast.Inspect(handle.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			name = fn.Name
		}
		assert.False(t, banned[name],
			"enrollListener.handle now calls %s(). The enrolment protocol is 'the producer sends nothing' "+
				"(shim/core/enroll.h), so a read here blocks until the producer's 2s budget expires and every "+
				"rendezvous becomes a 2s stall ending in kEnrollError - issue #49's ~38%% stack loss, by a new "+
				"route. A cubin offer needs a header; that is why it has its own socket (cubin.go).", name)
		return true
	})
}

// The header codec, round-tripped, so the 24 bytes the Go decoder expects are
// the 24 bytes the C++ producer writes. shim/core/cubin_test.cc pins the same
// offsets from the other side; between them a layout or endianness drift
// cannot pass unnoticed.
func TestTheOfferHeaderCodecRoundTrips(t *testing.T) {
	h := cubinHeader{magic: cubinHeaderMagic, version: cubinHeaderVersion, flags: 0,
		size: 0x0102030405060708, crc: 0x0F0E0D0C0B0A0908}
	raw := encodeCubinHeader(h)
	require.Len(t, raw, cubinHeaderSize)
	assert.Equal(t, []byte{'C', 'U', 'B', '1'}, raw[0:4])
	assert.Equal(t, byte(1), raw[4])
	assert.Equal(t, byte(0), raw[6])

	got, err := decodeCubinHeader(raw)
	require.NoError(t, err)
	assert.Equal(t, h, got)

	_, err = decodeCubinHeader(raw[:cubinHeaderSize-1])
	assert.Error(t, err, "a short header must be an error, not a zero-filled one")
}

// Two consumers cannot both own one shim's offer channel, for the same reason
// two cannot own its rendezvous: a consumer that thinks it has one and does
// not resolves nothing and says nothing.
func TestASecondCubinListenerForTheSameShimIsRefused(t *testing.T) {
	shim := selfExe(t)
	testCubinListener(t, Config{ShimPath: shim}, newRecordingCubinSink())
	_, err := newCubinListener(Config{ShimPath: shim}, newRecordingCubinSink())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bind")
}

// Closing releases a peer mid-offer rather than leaving it writing into a
// consumer that has gone, and is idempotent on a nil listener - the shape a
// consumer that could not bind one has.
func TestClosingTheCubinListenerIsSafeAndReleasesPeers(t *testing.T) {
	var nilListener *cubinListener
	assert.NoError(t, nilListener.close())
	assert.Equal(t, "", nilListener.address())
	assert.Equal(t, cubinStats{}, nilListener.snapshot())

	shim := selfExe(t)
	sink := newRecordingCubinSink()
	sink.gate = make(chan struct{})
	l, err := newCubinListener(Config{ShimPath: shim}, sink)
	require.NoError(t, err)

	body := cubinFixture(1024)
	fd := sealedCubinFD(t, body, cubinRequiredSeals)
	got := make(chan byte, 1)
	go func() { got <- offerCubin(t, l.address(), offerHeader(uint64(len(body)), 21), fd) }()
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the offer never reached the store")
	}
	close(sink.gate)
	require.NoError(t, l.close())

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("a peer stayed parked after the cubin listener closed")
	}
}

// The built-in store is bounded. It is what a consumer with no Config.Modules
// gets - see TestAConsumerWithNoModuleStoreKeepsTheBoundedPlaceholder - and an
// unbounded map fed by a profiled application is the same defect whoever owns
// it, so the bound is asserted on that path too.
func TestTheBuiltInCubinStoreIsBounded(t *testing.T) {
	s := newMemCubinStore(2)
	require.NoError(t, s.PutCubin(1, []byte("a")))
	require.NoError(t, s.PutCubin(2, []byte("b")))
	assert.Error(t, s.PutCubin(3, []byte("c")), "the placeholder store grew past its bound")

	// A duplicate is free even at the bound: it stores nothing new.
	assert.NoError(t, s.PutCubin(1, []byte("a")))
	assert.True(t, s.HasCubin(1))
	assert.False(t, s.HasCubin(3))
	b, ok := s.get(2)
	require.True(t, ok)
	assert.Equal(t, []byte("b"), b)

	assert.Equal(t, defaultCubinStoreCapacity, newMemCubinStore(0).cap)
}

// The seal-name spelling, because a rejection reason that says "0x2" instead
// of "F_SEAL_SHRINK" is one nobody decodes twice.
func TestSealNamesAreSpeltOut(t *testing.T) {
	assert.Equal(t, "F_SEAL_SHRINK", cubinSealNames(unix.F_SEAL_SHRINK))
	assert.Equal(t, "F_SEAL_SEAL|F_SEAL_WRITE", cubinSealNames(unix.F_SEAL_SEAL|unix.F_SEAL_WRITE))
	assert.Equal(t, "none", cubinSealNames(0))
	assert.Equal(t,
		"F_SEAL_SEAL|F_SEAL_SHRINK|F_SEAL_GROW|F_SEAL_WRITE", cubinSealNames(cubinRequiredSeals))
}

// The one seam neither side's own tests can cover: the wire between them.
//
// The offer channel has no negotiation in it. The producer computes the
// socket name from /proc/self/maps in C++ (shim/core/cubin.cc) and this end
// computes it from the same source in Go, and if the two spellings ever
// diverge every module silently goes unresolvable - with no error anywhere,
// because "nobody is listening" is also what an unprofiled process sees. The
// header is in the same position: 24 bytes written by a C++ struct and read
// by a Go decoder, with nothing on the wire to catch a layout drift.
//
// So this builds the real producer out of shim/core, points the real listener
// at it, and runs it. No BPF, no GPU, no privilege.
func TestTheCppProducerAndTheGoCubinListenerAgreeOnTheWire(t *testing.T) {
	cxx, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("no c++ in PATH; this test builds the shim's producer half")
	}
	core, err := filepath.Abs(filepath.Join("..", "shim", "core"))
	require.NoError(t, err)
	if _, serr := os.Stat(filepath.Join(core, "cubin.cc")); serr != nil {
		t.Skipf("shim/core not present: %v", serr)
	}

	// BOTH filesystems. The address embeds a device number, and stat(2) and
	// /proc/<pid>/maps report the SAME device on tmpfs and DIFFERENT devices
	// on btrfs - where the shim is actually built. A test that ran only under
	// TMPDIR would prove the two ends agree on the one filesystem where they
	// cannot disagree, which is exactly how the enrolment address shipped
	// broken. See repoTempDir.
	for _, tc := range []struct{ name, dir string }{
		{name: "source filesystem, where the shim is really built", dir: repoTempDir(t)},
		{name: "TMPDIR", dir: t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(tc.dir, "cubin_producer.cc")
			require.NoError(t, os.WriteFile(src, []byte(cubinProducerSrc), 0o600))
			bin := filepath.Join(tc.dir, "cubin_producer")
			build := exec.Command(cxx, "-std=c++17", "-O2", "-fvisibility=hidden",
				"-I", core, "-o", bin, src,
				filepath.Join(core, "cubin.cc"), filepath.Join(core, "enroll.cc"))
			if out, berr := build.CombinedOutput(); berr != nil {
				t.Skipf("could not build the producer half: %v\n%s", berr, out)
			}

			want, aerr := cubinAddress(bin)
			require.NoError(t, aerr)

			sink := newRecordingCubinSink()
			l := testCubinListener(t, Config{ShimPath: bin}, sink)
			require.Equal(t, want, l.address(), "the listener bound a name other than the derived one")

			out, rerr := exec.Command(bin).Output()
			require.NoError(t, rerr, "producer failed: %s", out)
			got := strings.Fields(strings.TrimSpace(string(out)))
			require.Len(t, got, 2, "producer output: %q", out)

			// Asserted before the outcome, so a mismatch reports the two
			// names rather than just "no-listener".
			assert.Equal(t, want, got[1],
				"the two ends derived DIFFERENT offer-channel names for the same file under %s. "+
					"consumer=%s producer=%s. They never exchange this string, so a mismatch makes every "+
					"counter on both sides read zero and every module unresolvable",
				tc.dir, want, got[1])
			assert.Equal(t, "accepted", got[0],
				"the producer's offer was not accepted even though both ends derived the same name (%s), "+
					"so this is the wire format rather than the address", want)

			// And the bytes: the same fixture, generated independently in C++
			// and in Go, keyed by the CRC the producer declared.
			stored, ok := sink.get(cubinProducerCRC)
			require.True(t, ok, "the offer was accepted and nothing was stored under the declared CRC")
			assert.True(t, bytes.Equal(cubinFixture(cubinProducerSize), stored),
				"the C++ producer's bytes are not the bytes this end stored")

			st := l.snapshot()
			assert.Equal(t, uint64(1), st.received)
			assert.Equal(t, uint64(cubinProducerSize), st.bytes)
			assertNoCubinRejections(t, st)
		})
	}
}

const (
	cubinProducerSize = 5 * 1024
	cubinProducerCRC  = 0xDEADBEEFCAFEF00D
)

// cubinProducerSrc is the shim's producer half in miniature: derive the name,
// build the same fixture cubinFixture builds, seal it, offer it, print the
// outcome. Kept out of the test body so the C++ braces cannot be confused for
// Go ones by anything editing this file.
//
// The fixture generator is duplicated rather than shared on purpose - two
// independent spellings of the same bytes is what makes "byte-identical" an
// assertion instead of a tautology.
const cubinProducerSrc = `
#include "cubin.h"
#include <cstdio>
#include <string>
int main() {
    char name[128];
    if (!perfagent::cubin_self_name(name, sizeof(name))) { printf("no-address -\n"); return 1; }
    std::string body(5 * 1024, '\0');
    const char magic[8] = {'\x7f', 'E', 'L', 'F', '\x02', '\x01', '\x01', '\x00'};
    for (int i = 0; i < 8; i++) body[i] = magic[i];
    for (size_t i = 8; i < body.size(); i++) {
        body[i] = (char)(unsigned char)(i * 31 + i / 251);
    }
    const perfagent::CubinOfferResult r = perfagent::cubin_offer(
        name, body.data(), body.size(), 0xDEADBEEFCAFEF00DULL,
        perfagent::cubin_timeout_ms(5000));
    printf("%s @%s\n", perfagent::cubin_offer_result_name(r), name);
    return 0;
}
`

// ---------------------------------------------------------------------------
// Task 5: the stub's fake module-load path, end to end over the real channel.
//
// This is the assertion that makes Task 5 verifiable without a GPU. The stub
// runs the SAME perfagent::CubinQueue the CUPTI adapter runs - same
// CubinView, same copy-inside-the-callback, same crc-over-the-copy, same
// offer-on-the-drain-thread - over a checked-in cubin from
// internal/cubin/testdata. Reusing that fixture rather than inventing bytes
// is what makes "the reader parses what the transport delivers" one set of
// bytes instead of two that agree by assumption.
//
// What it cannot prove is anything about CUPTI: that MODULE_LOADED fires at
// all, that cuptiGetCubinCrc() over our copy matches the PC records' cubinCrc,
// or that a cuModuleUnload leaves our copy intact. Those are on the RTX 3090.

// stubCubinFixture reads one of Task 1's checked-in cubins.
func stubCubinFixture(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("..", "internal", "cubin", "testdata", name)
	b, err := os.ReadFile(p)
	require.NoError(t, err, "the Task 1 fixture must be checked in")
	require.NotEmpty(t, b)
	return b
}

// stubCubinCRC is an independent Go spelling of stub.cc's stub_cubin_crc.
// Two spellings, so "the key the consumer stored is the key the producer
// meant" is an assertion rather than a read-back.
//
// It stands in for cuptiGetCubinCrc(), which needs a CUDA toolkit this side
// does not have. What the join requires is that one number names one set of
// bytes and that the same number reaches both ends; a content hash satisfies
// that exactly as CUPTI's unpublished polynomial does.
func stubCubinCRC(b []byte) uint64 {
	h := uint64(1469598103934665603)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	if h == 0 {
		return 1
	}
	return h
}

// buildStub builds shim/perfagent-gpu-stub and hands back a copy with an
// inode of its own. The inode is the point: the cubin address embeds it, so a
// private copy is what stops a concurrent stub run - a second developer, a CI
// job - offering into this test's listener.
func buildStub(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make unavailable")
	}
	shim, err := filepath.Abs(filepath.Join("..", "shim"))
	require.NoError(t, err)
	if _, serr := os.Stat(filepath.Join(shim, "stub", "stub.cc")); serr != nil {
		t.Skipf("shim/stub not present: %v", serr)
	}
	out, err := exec.Command("make", "-C", shim, "perfagent-gpu-stub").CombinedOutput()
	require.NoError(t, err, "build perfagent-gpu-stub: %s", out)

	src := filepath.Join(shim, "perfagent-gpu-stub")
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	dst := filepath.Join(repoTempDir(t), "perfagent-gpu-stub")
	require.NoError(t, os.WriteFile(dst, data, 0o700))
	return dst
}

// runStubWithCubins runs the stub with no launches at all, so the only thing
// it does is the fake module load. Returns its stderr.
func runStubWithCubins(t *testing.T, stub string, paths ...string) string {
	t.Helper()
	cmd := exec.Command(stub, "0", "0", "8", "0")
	cmd.Env = append(os.Environ(), "PERFAGENT_STUB_CUBINS="+strings.Join(paths, ":"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "stub failed: %s", stderr.String())
	return stderr.String()
}

func TestTheStubsFakeModuleLoadDeliversACheckedInCubin(t *testing.T) {
	stub := buildStub(t)
	fixture := stubCubinFixture(t, "single_lineinfo.cubin")
	want := stubCubinCRC(fixture)

	sink := newRecordingCubinSink()
	l := testCubinListener(t, Config{ShimPath: stub}, sink)

	path, err := filepath.Abs(filepath.Join("..", "internal", "cubin", "testdata", "single_lineinfo.cubin"))
	require.NoError(t, err)
	out := runStubWithCubins(t, stub, path)

	// The address first: when the two ends derive different names every
	// counter on both sides reads zero and nothing else in this test says why.
	assert.Contains(t, out, "cubin_addr=@"+strings.TrimPrefix(l.address(), "@"),
		"the stub derived a different cubin address than the listener bound")

	stored, ok := sink.get(want)
	require.True(t, ok, "nothing stored under the CRC the stub declared. stub said: %s", out)
	assert.True(t, bytes.Equal(fixture, stored),
		"the bytes the stub offered are not the fixture's bytes")

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.received)
	assert.Equal(t, uint64(len(fixture)), st.bytes)
	assertNoCubinRejections(t, st)

	// The producer's own accounting, which no consumer-side counter can see:
	// a module dropped before it ever reached the wire is upstream of
	// everything above.
	assert.Contains(t, out, "captured=1 reload_skipped=0 queue_full=0 too_large=0 "+
		"crc_failed=0 alloc_failed=0 sent=1 send_failed=0 pending=0")
}

func TestTheStubDeliversSeveralModulesInOneRun(t *testing.T) {
	stub := buildStub(t)
	sink := newRecordingCubinSink()
	l := testCubinListener(t, Config{ShimPath: stub}, sink)

	names := []string{"single_lineinfo.cubin", "single_nolineinfo.cubin", "two_kernels_lineinfo.cubin"}
	var paths []string
	total := 0
	for _, n := range names {
		p, err := filepath.Abs(filepath.Join("..", "internal", "cubin", "testdata", n))
		require.NoError(t, err)
		paths = append(paths, p)
		total += len(stubCubinFixture(t, n))
	}
	out := runStubWithCubins(t, stub, paths...)

	// A -lineinfo cubin and a no-lineinfo one in one run is what lets the
	// gate reach more than one gpu_src_status value from a single producer.
	for _, n := range names {
		fixture := stubCubinFixture(t, n)
		stored, ok := sink.get(stubCubinCRC(fixture))
		require.True(t, ok, "%s did not arrive. stub said: %s", n, out)
		assert.True(t, bytes.Equal(fixture, stored), "%s arrived with different bytes", n)
	}
	st := l.snapshot()
	assert.Equal(t, uint64(3), st.received)
	assert.Equal(t, uint64(total), st.bytes)
	assertNoCubinRejections(t, st)
}

// The adapter-side intern: CUDA's lazy loading re-loads modules, and a
// re-load of the same CRC must not re-offer. Counted on the producer, since
// the consumer would only ever see it as a duplicate.
func TestTheStubDoesNotReofferAReloadedModule(t *testing.T) {
	stub := buildStub(t)
	fixture := stubCubinFixture(t, "unrolled_lineinfo.cubin")

	sink := newRecordingCubinSink()
	l := testCubinListener(t, Config{ShimPath: stub}, sink)

	path, err := filepath.Abs(filepath.Join("..", "internal", "cubin", "testdata", "unrolled_lineinfo.cubin"))
	require.NoError(t, err)
	out := runStubWithCubins(t, stub, path, path, path)

	assert.Contains(t, out, "captured=1 reload_skipped=2")
	st := l.snapshot()
	assert.Equal(t, uint64(1), st.received)
	assert.Equal(t, uint64(len(fixture)), st.bytes)
	// A re-offer would have shown up here rather than as a rejection, so the
	// duplicate counter is checked at zero too: the producer suppressed it,
	// the consumer never had to.
	assert.Zero(t, st.duplicate, "the consumer had to absorb a re-offer the producer should have suppressed")
	assertNoCubinRejections(t, st)
}

// The boring run must read zero everywhere. Eleven defects on this project
// were counters reading green exactly when things were worst, so the
// direction that matters is asserted in both.
func TestAStubRunWithNoModulesTouchesTheCubinChannelAtAll(t *testing.T) {
	stub := buildStub(t)
	sink := newRecordingCubinSink()
	l := testCubinListener(t, Config{ShimPath: stub}, sink)

	cmd := exec.Command(stub, "0", "0", "8", "0")
	cmd.Env = append(os.Environ(), "PERFAGENT_STUB_CUBINS=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "stub failed: %s", stderr.String())

	assert.Contains(t, stderr.String(), "cubins requested=0 captured=0 reload_skipped=0 "+
		"queue_full=0 too_large=0 crc_failed=0 alloc_failed=0 sent=0 send_failed=0 pending=0")
	st := l.snapshot()
	assert.Zero(t, st.received)
	assert.Zero(t, st.bytes)
	assert.Zero(t, st.mapped)
	assertNoCubinRejections(t, st)
}

// Issue #96. The total ceiling used to be charged against a cumulative tally of
// every byte ever accepted, while the bytes themselves live in a store bounded
// by MaxBytes with LRU eviction. The tally climbed forever and resident usage
// stayed flat, so a process loading more than the ceiling in DISTINCT cubins
// over its lifetime began refusing offers while the agent held almost nothing.
//
// The sink here models exactly that: it accepts everything and reports a
// resident size that never grows, which is what an evicting store looks like
// from the transport's side.
func TestTheTotalCeilingIsChargedAgainstResidentNotLifetimeBytes(t *testing.T) {
	shim := selfExe(t)
	sink := newRecordingCubinSink()
	sink.evicting = true // everything stored is immediately evicted
	const each = 32 * 1024
	l := testCubinListener(t, Config{ShimPath: shim, CubinTotalBytes: 3 * each}, sink)

	// Ten distinct cubins, together well past the three-cubin ceiling. Every
	// one must be accepted, because the store is holding none of them.
	for i := range 10 {
		body := cubinFixture(each)
		body[8] = byte(i)
		crc := crc64.Checksum(body, cubinCRCTable)
		fd := sealedCubinFD(t, body, cubinRequiredSeals)
		require.Equal(t, byte(cubinReplyOK), offerCubin(t, l.address(), offerHeader(each, crc), fd),
			"offer %d: the store holds nothing, so the ceiling cannot be reached", i)
	}

	st := l.snapshot()
	assert.Equal(t, uint64(10), st.received)
	assert.Zero(t, st.tooLarge, "no offer may be refused while the store is empty")
	// The lifetime counter still climbs — it is a useful figure and stays a
	// counter. It is simply no longer the bound.
	assert.Equal(t, uint64(10*each), st.bytes,
		"CubinBytesReceived remains cumulative; only its use as a limit changed")
}

// The other half: when the store really is holding the bytes, the ceiling
// still bites. Without this the test above would pass against a build that
// removed the ceiling altogether.
func TestTheTotalCeilingStillRefusesWhenTheStoreIsActuallyFull(t *testing.T) {
	shim := selfExe(t)
	sink := newRecordingCubinSink() // evicting=false: resident == lifetime
	const each = 32 * 1024
	l := testCubinListener(t, Config{ShimPath: shim, CubinTotalBytes: 2 * each}, sink)

	for i := range 2 {
		body := cubinFixture(each)
		body[8] = byte(i)
		crc := crc64.Checksum(body, cubinCRCTable)
		fd := sealedCubinFD(t, body, cubinRequiredSeals)
		require.Equal(t, byte(cubinReplyOK), offerCubin(t, l.address(), offerHeader(each, crc), fd))
	}

	over := cubinFixture(each)
	over[8] = 0xFE
	overCRC := crc64.Checksum(over, cubinCRCTable)
	overFD := sealedCubinFD(t, over, cubinRequiredSeals)
	assert.Equal(t, byte(cubinReplyRefused),
		offerCubin(t, l.address(), offerHeader(each, overCRC), overFD))

	st := l.snapshot()
	assert.Equal(t, uint64(1), st.tooLarge)
	assert.Contains(t, st.lastErr, "total ceiling")
}
