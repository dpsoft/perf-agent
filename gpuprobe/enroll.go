package gpuprobe

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// The consumer half of the startup rendezvous. shim/core/enroll.h is the
// producer half and carries the full rationale; the short version is that the
// stack walk happens in the kernel, inside walk_step, at the instant the
// uprobe traps, so the CFI tables have to be in the BPF maps before the probe
// can fire. Nothing a consumer observes in userspace - a first batch, an mmap
// notification, an exec notification - happens before that, so registration
// driven by any of them is definitionally late: the sample that triggered it
// was already walked without tables, and so is everything that arrives during
// the compile. On an RTX 3090 that compile is ~73ms for libcuda's 135805 CFI
// entries against a ~540ms workload, which is the ~38% of sampled stacks
// issue #49 measured as lost.
//
// So the producer waits instead. A CUDA process's adapter is dlopened by the
// driver during cuInit, which is after libcuda and the application are mapped
// and before any kernel launch, and it blocks there until this listener has
// installed its tables. The window is closed rather than narrowed: there is
// no launch, therefore no probe, therefore no walk, until the reply goes out.
//
// # The address
//
// An abstract AF_UNIX name both ends derive independently from the shim's own
// device and inode:
//
//	@perfagent-gpu-enroll.v1.<dev_major>.<dev_minor>.<inode>
//
// The inode is the key because it is what a uprobe attaches to: a producer
// whose probes this consumer armed maps that inode by construction, and two
// consumers watching two different copies of the shim - the gate makes a
// private copy per run precisely so concurrent runs cannot collide - get two
// different names for free. No environment variable, no path agreement, and
// no cooperation from whoever launched the profiled process.
//
// # What it will not do
//
// The peer's PID comes from SO_PEERCRED, which the kernel fills in and no
// payload can forge, and the request is refused unless that PID actually maps
// the shim inode this consumer attached to. An arbitrary local process
// therefore cannot make a root profiler compile CFI for a PID of its
// choosing; the most it can do is re-enroll a process that is already being
// profiled, which costs a map lookup because pidRegistry.enroll never
// recompiles a PID that already holds tables.
//
// That leaves resource exhaustion rather than misdirection: the address is
// reachable by any local user who can read and map the shim, serve() is
// serial, and each admitted enrolment costs a /proc/<pid>/maps read plus a
// pid_mappings population - a fork loop of such processes could occupy the
// listener, expire genuine producers' budgets and churn the registry's
// bounded LRU. (The compile itself is largely amortised: TableStore keys on
// build-id, so a thousand processes mapping the same libLLVM pay for one.)
// enrollAdmission bounds it: a token bucket per peer uid and one for the
// listener as a whole, both refilled continuously, with refusal being
// instantaneous and counted in Stats.UnwindEnrollThrottled. A throttled
// producer is released immediately and takes the lazy path - the failure mode
// is a worse profile, never a stalled application.
//
// # The TOCTOU, and why it is benign
//
// SO_PEERCRED is stamped at connect time, but /proc/<pid>/maps is read
// afterwards, so in principle the peer could exit and its PID be recycled in
// between. In practice the peer is blocked reading the socket for the whole
// window, and even when it is not, the consequence is bounded: the maps read
// and the registration use the SAME later /proc read, so the tables installed
// are correct for whoever holds that PID now, not stale ones for the process
// that connected. Worse, that new process must ITSELF map the shim inode or
// the check refuses it - which makes it a legitimate producer. The cost of
// losing this race is therefore a wasted compile and an LRU slot, never a
// wrong stack. Accepted and documented rather than papered over with a
// pidfd dance that would still not be atomic against exec.
//
// # It is never fatal
//
// If the address cannot be bound - another consumer already has it, or
// abstract sockets are unavailable - Attach continues without a listener and
// every producer falls back to lazy registration, i.e. exactly the pre-#49
// behaviour. Stats.UnwindEnrollListening says which of the two a run had, so
// a degraded run is legible rather than silent.

// enrollReply is the single status byte the producer waits for.
const (
	enrollReplyOK      = 'K' // the walker has this PID's tables
	enrollReplyRefused = 'X' // it does not, and will not before you run
)

// enrollReplyTimeout bounds the reply write, and nothing else.
//
// Deliberately not a deadline over the whole handler: registration is a CFI
// compile, and a deadline set before it would expire during a legitimately
// slow one (a process mapping something libLLVM-sized) and then swallow the
// reply that the compile had just earned. The producer's own budget
// (PERFAGENT_GPU_ENROLL_TIMEOUT_MS, default 2s) is what bounds how long it
// waits; this end always tries to answer.
//
// One byte into a socket whose peer is blocked reading it cannot actually
// block, so this only ever fires for a producer that has already given up and
// gone - which is exactly when the accept loop must not be held.
const enrollReplyTimeout = 5 * time.Second

// The admission bucket. Sized so that a legitimate burst is never throttled
// and a fork loop is: 32 distinct CUDA processes started by one user inside a
// second is already far past any real workload, and an attacker's loop does
// thousands. A throttled producer is refused instantly and runs on the lazy
// path, so the cost of being wrong in the tight direction is a worse profile
// for that process, not a stalled one.
const (
	enrollUIDBurst    = 32
	enrollUIDRefill   = 32 // tokens per second
	enrollTotalBurst  = 96
	enrollTotalRefill = 96
	// enrollUIDTrackMax bounds the per-uid table itself, so the defence
	// cannot become the exhaustion. Beyond it, everything falls back to the
	// listener-wide bucket alone.
	enrollUIDTrackMax = 64
)

// enrollAdmission is a continuously refilled token bucket, one per peer uid
// plus one for the listener as a whole. Not safe for concurrent use; serve()
// is serial and calls it from one goroutine.
type enrollAdmission struct {
	now func() time.Time

	total    float64
	totalAt  time.Time
	perUID   map[uint32]*enrollBucket
	burst    float64
	refill   float64
	uidBurst float64
	uidFill  float64
}

type enrollBucket struct {
	tokens float64
	at     time.Time
}

func newEnrollAdmission(now func() time.Time) *enrollAdmission {
	if now == nil {
		now = time.Now
	}
	return &enrollAdmission{
		now:      now,
		total:    enrollTotalBurst,
		totalAt:  now(),
		perUID:   map[uint32]*enrollBucket{},
		burst:    enrollTotalBurst,
		refill:   enrollTotalRefill,
		uidBurst: enrollUIDBurst,
		uidFill:  enrollUIDRefill,
	}
}

// admit spends one token from the uid's bucket and one from the listener's,
// and reports whether both had one. Neither is charged unless both could pay,
// so a uid that is already throttled cannot drain the shared bucket.
func (a *enrollAdmission) admit(uid uint32) bool {
	now := a.now()
	a.total = refillTokens(a.total, a.totalAt, now, a.refill, a.burst)
	a.totalAt = now

	b, tracked := a.perUID[uid]
	if !tracked {
		if len(a.perUID) >= enrollUIDTrackMax {
			// Reclaim first. A bucket that has refilled to its burst has not
			// been spent for a full window, so it is indistinguishable from
			// one that was never created: dropping it forgets nothing.
			// Without this, sixty-four one-shot uids would pin the table
			// forever and push every later producer - including the real one
			// - onto the shared bucket alone.
			a.sweepIdle(now)
		}
		if len(a.perUID) >= enrollUIDTrackMax {
			// Still full: every tracked uid is actively spending. Fall back
			// to the listener-wide bucket rather than growing without bound
			// or refusing outright.
			if a.total < 1 {
				return false
			}
			a.total--
			return true
		}
		b = &enrollBucket{tokens: a.uidBurst, at: now}
		a.perUID[uid] = b
	}
	b.tokens = refillTokens(b.tokens, b.at, now, a.uidFill, a.uidBurst)
	b.at = now

	if a.total < 1 || b.tokens < 1 {
		return false
	}
	a.total--
	b.tokens--
	return true
}

// sweepIdle drops every per-uid bucket that has refilled completely, i.e.
// every uid that has not spent a token for a full window. Their state carries
// no information a fresh bucket would not have.
func (a *enrollAdmission) sweepIdle(now time.Time) {
	for uid, b := range a.perUID {
		if refillTokens(b.tokens, b.at, now, a.uidFill, a.uidBurst) >= a.uidBurst {
			delete(a.perUID, uid)
		}
	}
}

func refillTokens(tokens float64, since, now time.Time, perSec, max float64) float64 {
	if d := now.Sub(since); d > 0 {
		tokens += d.Seconds() * perSec
	}
	if tokens > max {
		tokens = max
	}
	return tokens
}

// enrollAddress is the rendezvous name for shimPath. Both ends compute it
// from the same two numbers the kernel reports for the file, so they agree
// without exchanging anything.
func enrollAddress(shimPath string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(shimPath, &st); err != nil {
		return "", fmt.Errorf("stat shim %s: %w", shimPath, err)
	}
	dev := uint64(st.Dev) //nolint:unconvert // st.Dev is uint64 on amd64, uint32-widened elsewhere
	return fmt.Sprintf("@perfagent-gpu-enroll.v1.%d.%d.%d",
		unix.Major(dev), unix.Minor(dev), st.Ino), nil
}

// enrollStats is the listener's slice of gpuprobe.Stats, under its own lock.
type enrollStats struct {
	requests  uint64
	confirmed uint64
	refused   uint64
	throttled uint64
	failed    uint64
	lastErr   string
}

// enrollListener serves the producer-side rendezvous for one consumer.
type enrollListener struct {
	ln  *net.UnixListener
	reg *pidRegistry
	// pid mirrors Config.PID: non-zero means this consumer's uprobes are
	// filtered to one process, so an enrollment from any other PID is
	// refused. The socket name is not PID-scoped and cannot be.
	pid uint32
	// dev and ino identify the shim image a peer must have mapped. Read once
	// at construction: the file the uprobes are attached to cannot change
	// identity underneath a live link.
	dev, ino uint64
	// procRoot is "/proc" in production; tests point it at a fixture.
	procRoot string
	// requireUID, when set, is the only peer uid this listener will serve.
	// Nil for a privileged consumer, which legitimately profiles other
	// users' processes. See enrollRequiredUID.
	requireUID *uint32
	// admit bounds how fast one peer uid, and the listener as a whole, may
	// spend the single serving goroutine. Touched only from serve().
	admit *enrollAdmission

	wg sync.WaitGroup

	mu    sync.Mutex
	stats enrollStats
}

// newEnrollListener binds the rendezvous for cfg's shim and starts serving.
//
// Must be called BEFORE the uprobe_multi link is created. Creating the link
// is what arms the shim's semaphores, and an armed semaphore is what makes a
// producer attempt the rendezvous; binding afterwards would leave a window in
// which a producer starts, finds nothing listening, and takes the lazy path -
// which is the bug.
func newEnrollListener(cfg Config, reg *pidRegistry) (*enrollListener, error) {
	l, err := buildEnrollListener(cfg, reg)
	if err != nil {
		return nil, err
	}
	l.start()
	return l, nil
}

// buildEnrollListener binds without serving. Split out so a test can settle
// state that serve() alone is allowed to touch afterwards - `admit` has no
// lock precisely because only the serving goroutine reaches it, and a test
// that reached in while it ran would be racing the code it is checking.
func buildEnrollListener(cfg Config, reg *pidRegistry) (*enrollListener, error) {
	addr, err := enrollAddress(cfg.ShimPath)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Stat(cfg.ShimPath, &st); err != nil {
		return nil, fmt.Errorf("stat shim %s: %w", cfg.ShimPath, err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", addr, err)
	}
	l := &enrollListener{
		ln:         ln,
		reg:        reg,
		pid:        uint32(cfg.PID),
		dev:        uint64(st.Dev), //nolint:unconvert // see enrollAddress
		ino:        st.Ino,
		procRoot:   "/proc",
		admit:      newEnrollAdmission(nil),
		requireUID: enrollRequiredUID(),
	}
	return l, nil
}

// start begins accepting. Separate from construction so buildEnrollListener's
// caller can finish arranging the listener before any peer can reach it.
func (l *enrollListener) start() {
	l.wg.Add(1)
	go l.serve()
}

// serve accepts one producer at a time.
//
// Serial on purpose. Each connection's work is a CFI compile, and the
// registry's own worker goroutine may be doing another; ehmaps' store and
// tracker are mutex-protected so concurrency would be safe, but it would not
// be useful - the cost is I/O-bound on reading .eh_frame, and N producers
// starting at once would only interleave into the same total. Serial keeps
// the ordering property this file exists for trivially true: the reply for a
// producer goes out after that producer's tables are installed, and nothing
// else can be half-installed at the time.
func (l *enrollListener) serve() {
	defer l.wg.Done()
	for {
		conn, err := l.ln.AcceptUnix()
		if err != nil {
			// The only expected error is the listener being closed.
			return
		}
		l.handle(conn)
	}
}

// handle registers one producer and then releases it. Every early return
// leaves the producer released (the deferred Close is an EOF the producer
// reads as "no promise"), because a producer parked on a consumer that fell
// over is worse than a producer with frame-pointer stacks.
func (l *enrollListener) handle(conn *net.UnixConn) {
	defer func() { _ = conn.Close() }()

	pid, uid, err := enrollPeerCreds(conn)
	if err != nil {
		// No credentials means no PID to register and none to refuse by
		// name. Counted as a refusal so the connection is never invisible.
		l.refuse(conn, "peer credentials: "+err.Error())
		return
	}
	// Cheapest check first, and before any /proc I/O: a caller that has
	// exhausted its budget must not be able to spend the listener's time on
	// the checks either.
	if !l.admit.admit(uid) {
		l.mu.Lock()
		l.stats.throttled++
		l.stats.lastErr = fmt.Sprintf("uid %d throttled at pid %d; enrolment rate exceeded", uid, pid)
		l.mu.Unlock()
		l.reply(conn, enrollReplyRefused)
		return
	}
	if l.requireUID != nil && uid != *l.requireUID {
		l.refuse(conn, fmt.Sprintf("pid %d has uid %d; this consumer is unprivileged and serves only uid %d",
			pid, uid, *l.requireUID))
		return
	}
	// Two independent reasons to refuse, both of which mean "this is not a
	// process whose stacks this consumer walks".
	if l.pid != 0 && pid != l.pid {
		l.refuse(conn, fmt.Sprintf("pid %d (uid %d) is not the attached pid %d", pid, uid, l.pid))
		return
	}
	mapped, err := procMapsHaveInode(l.procRoot, pid, l.dev, l.ino)
	if err != nil {
		l.refuse(conn, fmt.Sprintf("read maps for pid %d: %v", pid, err))
		return
	}
	if !mapped {
		l.refuse(conn, fmt.Sprintf("pid %d (uid %d) does not map the shim (dev %d:%d ino %d)",
			pid, uid, unix.Major(l.dev), unix.Minor(l.dev), l.ino))
		return
	}

	l.mu.Lock()
	l.stats.requests++
	l.mu.Unlock()

	// The compile. The producer is blocked in its own init for the whole of
	// it, which is the point: nothing it does can reach a probe until the
	// reply below goes out.
	outcome, rerr := l.reg.enroll(pid)
	switch outcome {
	case enrollInstalled, enrollAlreadyHeld:
		l.mu.Lock()
		l.stats.confirmed++
		l.mu.Unlock()
		l.reply(conn, enrollReplyOK)
	default:
		// The rendezvous happened and the tables still are not there. The
		// producer is released rather than parked - see the file comment -
		// and every stack it goes on to take is counted twice, once in
		// StacksWalkedNoTables and once in StacksNoTablesAfterEnroll.
		msg := "registration installed nothing"
		if rerr != nil {
			msg = rerr.Error()
		}
		l.mu.Lock()
		l.stats.failed++
		l.stats.lastErr = msg
		l.mu.Unlock()
		l.reply(conn, enrollReplyRefused)
	}
}

// refuse turns a connection away without registering anything: it is not a
// producer this consumer is responsible for, or it could not be identified.
func (l *enrollListener) refuse(conn *net.UnixConn, reason string) {
	l.mu.Lock()
	l.stats.refused++
	l.stats.lastErr = reason
	l.mu.Unlock()
	l.reply(conn, enrollReplyRefused)
}

// reply hands the producer its status byte. A failed write is not worth
// retrying: the producer's own timeout releases it either way.
func (l *enrollListener) reply(conn *net.UnixConn, b byte) {
	_ = conn.SetWriteDeadline(time.Now().Add(enrollReplyTimeout))
	_, _ = conn.Write([]byte{b})
}

// snapshot copies the counters out for Stats.
func (l *enrollListener) snapshot() enrollStats {
	if l == nil {
		return enrollStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

// close stops accepting and waits for the goroutine. Safe on a nil listener
// (Attach could not bind one) and idempotent.
func (l *enrollListener) close() error {
	if l == nil {
		return nil
	}
	err := l.ln.Close()
	l.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// enrollRequiredUID returns the single peer uid this consumer will serve, or
// nil when it will serve any.
//
// The uid is deliberately NOT a blanket authorisation check. A root profiler
// legitimately profiles processes belonging to other users - refusing
// anything but its own euid would turn the rendezvous off in the commonest
// production shape and silently restore the pre-#49 loss. What authorises a
// peer in general is procMapsHaveInode: it maps the very inode this
// consumer's uprobes are attached to.
//
// The exception is strictly additive. A consumer running as a non-root user
// WITHOUT CAP_SYS_PTRACE cannot read another user's /proc/<pid>/maps at all,
// so it could never have served that producer: pinning it to its own uid
// refuses nothing it could have done, and closes the rendezvous to every
// other local user on a multi-user box. Privileged consumers - root, or
// setcap'd with CAP_SYS_PTRACE - keep the inode check alone.
//
// Permitted as well as Effective, for the same reason perfagent's
// hasCapSysPtrace checks both: a setcap'd binary has not promoted Permitted
// yet, and gating on Effective alone would misread it as unprivileged.
func enrollRequiredUID() *uint32 {
	if os.Geteuid() == 0 {
		return nil
	}
	if caps := cap.GetProc(); caps != nil {
		for _, flag := range []cap.Flag{cap.Permitted, cap.Effective} {
			if have, err := caps.GetFlag(flag, cap.SYS_PTRACE); err == nil && have {
				return nil
			}
		}
	}
	uid := uint32(os.Geteuid())
	return &uid
}

// enrollPeerCreds reads the connecting process's PID and uid out of
// SO_PEERCRED. The kernel stamps both at connect time from the peer's own
// task, so neither can be forged by anything the peer sends.
//
// The uid is the accounting key for the rate limiter, a name in the error
// string when something is refused, and - only for an unprivileged consumer -
// the check in enrollRequiredUID.
func enrollPeerCreds(conn *net.UnixConn) (pid, uid uint32, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var ucred *unix.Ucred
	var inner error
	if cerr := raw.Control(func(fd uintptr) {
		ucred, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); cerr != nil {
		return 0, 0, cerr
	}
	if inner != nil {
		return 0, 0, inner
	}
	if ucred == nil || ucred.Pid <= 0 {
		return 0, 0, errors.New("no peer pid")
	}
	return uint32(ucred.Pid), ucred.Uid, nil
}

// procMapsHaveInode reports whether pid maps the file identified by dev and
// ino. This is the same identity a uprobe attaches to, so it answers exactly
// the question that matters: is this a process whose probes this consumer
// armed?
//
// procRoot is a parameter so the parsing can be tested against a fixture
// without a live process of a known layout.
func procMapsHaveInode(procRoot string, pid uint32, dev, ino uint64) (bool, error) {
	f, err := os.Open(fmt.Sprintf("%s/%d/maps", procRoot, pid))
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	wantMaj, wantMin := unix.Major(dev), unix.Minor(dev)
	sc := bufio.NewScanner(f)
	// A CUDA process's maps is a few hundred lines; the default 64KiB token
	// limit is far past the longest of them.
	for sc.Scan() {
		// 7fc9a0a00000-7fc9a0a21000 r-xp 00001000 08:02 4242 /lib/libshim.so
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		gotIno, err := strconv.ParseUint(fields[4], 10, 64)
		// Inode 0 is an anonymous mapping - the stack, the heap, a private
		// arena. No uprobe can be attached to one, so it can never be the
		// shim, and matching it would turn "the shim is not mapped" into
		// "the shim is mapped" for any caller that passed a zero inode.
		if err != nil || gotIno == 0 || gotIno != ino {
			continue
		}
		maj, min, ok := parseMapsDev(fields[3])
		if !ok || maj != wantMaj || min != wantMin {
			continue
		}
		return true, nil
	}
	return false, sc.Err()
}

// parseMapsDev splits the "major:minor" field of a /proc/<pid>/maps line,
// which is hex, into the decimal numbers unix.Major/unix.Minor produce.
func parseMapsDev(field string) (uint32, uint32, bool) {
	maj, min, ok := strings.Cut(field, ":")
	if !ok {
		return 0, 0, false
	}
	mj, err := strconv.ParseUint(maj, 16, 32)
	if err != nil {
		return 0, 0, false
	}
	mn, err := strconv.ParseUint(min, 16, 32)
	if err != nil {
		return 0, 0, false
	}
	return uint32(mj), uint32(mn), true
}
