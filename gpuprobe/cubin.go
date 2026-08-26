package gpuprobe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/gpu"
)

// The cubin transport: how a module's bytes get from the producer to this
// consumer, on a channel that is NOT the enrolment rendezvous.
//
// # Why the bytes have to travel at all
//
// A gpu_pc_sample_batch_v1 record keys on cubin_crc, carries a pc_offset and
// a function_index, and names nothing. There is no kernel_id on it and no
// table on the wire that maps (cubin_crc, function_index) to anything a
// reader could act on. The cubin IS that table: its .symtab names the
// functions and its .debug_line turns a pc_offset into a source line. Without
// the bytes a Tier B PC sample is four integers, and the profile is honest and
// useless.
//
// gpu_module_load_v1 carries a bytes_ptr, and it is not a transport. It is a
// pointer into the PRODUCER's address space; reading it from here needs
// /proc/<pid>/mem or process_vm_readv, both of which need CAP_SYS_PTRACE,
// which this agent's capability set deliberately does not contain and will
// not grow to contain. Reading it in BPF is not an alternative either: the
// ringbuf reservation is a compile-time 3072-byte payload, so a 512 KB cubin
// would need ~170 ordered, reassembled batches per module out of a uprobe
// with a bounded instruction budget. So the bytes take their own road, and
// gpu_module_load_v1 stays exactly as frozen - it announces THAT a module
// loaded, with its CRC and its size, and nothing about it changes here.
//
// # Why this is not the enrolment socket, in four parts
//
// shim/core/enroll.h states the protocol as "The producer sends nothing", and
// enrollListener.handle implements exactly that: creds, admit, uid and pid
// checks, procMapsHaveInode, reg.enroll, one status byte. It NEVER reads. A
// cubin offer must send a header, so a shared address would force this end to
// read in order to tell an offer from an enrolment - and on a genuine
// enrolment that read blocks until the producer's own 2s budget expires and
// it closes. Every rendezvous would become a 2s stall ending in
// kEnrollError, which is issue #49's ~38% stack loss arriving by a new route.
// TestAnEnrolmentStillCompletesWithoutAnyReadOnThatConnection pins the
// absence of that read.
//
// Three more, none of them hypothetical:
//
//   - enrollListener.serve() is serial AT Accept, not merely at handle. Offer
//     connections would be accepted by the same loop, so a queue of offers -
//     or one 2 MB stream - is served FIFO AHEAD of a genuine producer whose
//     connect() already landed in the backlog. Ahead is the dangerous
//     direction; behind is the easy one.
//   - Admission is charged per connection against enrollUIDBurst = 32,
//     refilling 32/s. A JIT- or template-heavy workload loading more than 32
//     modules a second would drain the bucket and then have its genuine
//     ENROLMENTS refused, with only UnwindEnrollThrottled moving.
//   - It would put a connect() and an up-to-2 MB write on the application's
//     cuModuleLoad path. The application is never stalled for the profiler's
//     benefit; the shim's copy happens in the callback and the offer happens
//     on the drain thread.
//
// The answer is a second channel with its own address, its own listener, its
// own goroutine and its own admission bucket. That is what makes every hazard
// above structurally impossible rather than test-defended: a throttled cubin
// offer cannot spend an enrolment token because it is not the same bucket,
// and a queued offer cannot spend an enrolment Accept slot because it is not
// the same listener. What IS reused, verbatim, is the authentication -
// enrollPeerCreds and procMapsHaveInode - and the shim-identity derivation
// (enrollShimIdentity), so a cubin offer is authenticated exactly as an
// enrolment is and the address inherits the btrfs stat-versus-maps fix
// instead of reintroducing that bug.
//
// # The payload is a sealed memfd, and the seals are load-bearing
//
// The producer creates a memfd, writes the cubin into it, applies
// F_SEAL_SEAL|F_SEAL_SHRINK|F_SEAL_GROW|F_SEAL_WRITE, and passes the
// descriptor by SCM_RIGHTS beside a 24-byte header. This end mmaps it. The
// bytes therefore never stream through the socket and the producer never
// blocks on this end's read rate, which is the property the drain thread
// needs.
//
// Each seal answers a specific way an unsealed fd would be a weapon:
//
//   - without F_SEAL_SHRINK a peer can ftruncate the file under our mmap and
//     the next touched page SIGBUSes THIS PROCESS. A profiler that a profiled
//     process can kill is worse than no profiler;
//   - without F_SEAL_WRITE the ELF mutates under the parser mid-parse, so the
//     line table we derive describes bytes that no longer exist;
//   - without F_SEAL_GROW the size we validated is not the size we map;
//   - without F_SEAL_SEAL any of the other three can be removed again, which
//     makes checking them meaningless.
//
// So all four are required, they are verified with fcntl(F_GET_SEALS) BEFORE
// anything is mapped, and a missing seal is a counted rejection with no
// fallback that reads it anyway. Falling back is how a defended path becomes
// an undefended one.
//
// # Bounds
//
// A per-cubin ceiling and a total-bytes ceiling, both configurable, both
// counted when they bite. An oversized cubin is rejected WHOLE. It is never
// truncated to fit, because a truncated cubin parses into a wrong line table,
// and a wrong line table is the one failure worse than no line table.

// The cubin offer wire format. Fixed size, little-endian, naturally aligned,
// exactly like every record on the USDT wire - and mirrored by
// perfagent::CubinOfferHeader in shim/core/cubin.h, whose static_asserts pin
// the same numbers from the other side.
//
//	offset  size  field
//	     0     4  magic    'C' 'U' 'B' '1'
//	     4     2  version  1
//	     6     2  flags    0; reserved, and rejected when non-zero
//	     8     8  size     declared cubin length in bytes
//	    16     8  crc      cuptiGetCubinCrc() over those bytes
//
// The CRC is the producer's key, not a checksum this end recomputes: CUPTI's
// polynomial is not published and the agent has no CUDA toolkit. It is what
// gpu_pc_sample_batch_v1 records join on, so what matters is that the same
// number reaches both - which is a hardware check, stated as one.
const (
	cubinHeaderSize    = 24
	cubinHeaderMagic   = uint32('C') | uint32('U')<<8 | uint32('B')<<16 | uint32('1')<<24
	cubinHeaderVersion = 1
)

// The reply, one byte, same spelling as the enrolment reply so a producer
// that already knows one wire does not have to learn a second.
const (
	cubinReplyOK      = 'K' // the bytes are in; this CRC is resolvable
	cubinReplyRefused = 'X' // they are not, for one of the counted reasons
)

// cubinRequiredSeals is the set that must ALL be present. Not a preference.
const cubinRequiredSeals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK |
	unix.F_SEAL_GROW | unix.F_SEAL_WRITE

// cubinIOTimeout bounds the header read and the reply write on one offer.
//
// Unlike the enrolment reply's deadline this one genuinely matters: the
// producer here is not blocked in its own init waiting for us, so a peer that
// connects and then sends nothing would otherwise hold the serial accept loop
// forever. It cannot reach the enrolment listener from here - that is the
// point of the separation - but it could still starve every other module's
// resolution, so it is bounded.
const cubinIOTimeout = 2 * time.Second

// The cubin admission bucket. Its own numbers, not the enrolment ones.
//
// Enrolment is once per process, so 32/s is generous there. Module loads are
// per cuModuleLoad and CUDA's lazy loading clusters them, so the same 32
// would throttle an ordinary template-heavy workload. These are sized for a
// drain tick's worth of modules at a time (the shim offers from the 100ms
// drain thread, in batches) with a second's worth of refill behind it, while
// still bounding a fork loop to a few hundred refusals a second.
//
// Being wrong in the tight direction costs a module's source resolution -
// gpu_src_status reads "no-module" and the sample is still counted, still
// attributed and still honest. It cannot cost an enrolment.
const (
	cubinUIDBurst    = 128
	cubinUIDRefill   = 128 // tokens per second
	cubinTotalBurst  = 256
	cubinTotalRefill = 256
)

// Ceilings. Defaults, both overridable through Config.
const (
	// defaultCubinMaxBytes bounds one cubin. A -lineinfo cubin for a
	// realistic kernel set runs from a few KB to a few hundred KB; 8 MiB is
	// far past any of them and still small enough that the worst single
	// mapping is unremarkable.
	defaultCubinMaxBytes = 8 << 20
	// defaultCubinTotalBytes bounds every cubin this consumer will ever
	// accept. This is the memory a JIT- or template-explosion workload can
	// make the agent hold, so it is a hard ceiling rather than a dial.
	defaultCubinTotalBytes = 256 << 20
	// defaultCubinStoreCapacity bounds the number of distinct CRCs the
	// built-in store holds - the one a consumer with no Config.Modules gets,
	// where the bytes land and nothing reads them. A consumer that resolves
	// source lines supplies gpu.ModuleStore instead and is bounded by
	// gpu.ModuleStoreConfig. The bound exists on both paths because an
	// unbounded map fed by a profiled application is the same defect whoever
	// owns it.
	defaultCubinStoreCapacity = 512
)

// cubinHeader is the decoded wire header.
type cubinHeader struct {
	magic   uint32
	version uint16
	flags   uint16
	size    uint64
	crc     uint64
}

// decodeCubinHeader reads the fixed 24-byte header. It validates only what is
// structural; the ceilings and the seals are the caller's, in that order.
func decodeCubinHeader(b []byte) (cubinHeader, error) {
	if len(b) < cubinHeaderSize {
		return cubinHeader{}, fmt.Errorf("short header: %d of %d bytes", len(b), cubinHeaderSize)
	}
	h := cubinHeader{
		magic:   binary.LittleEndian.Uint32(b[0:4]),
		version: binary.LittleEndian.Uint16(b[4:6]),
		flags:   binary.LittleEndian.Uint16(b[6:8]),
		size:    binary.LittleEndian.Uint64(b[8:16]),
		crc:     binary.LittleEndian.Uint64(b[16:24]),
	}
	if h.magic != cubinHeaderMagic {
		return h, fmt.Errorf("bad magic %#08x, want %#08x", h.magic, cubinHeaderMagic)
	}
	if h.version != cubinHeaderVersion {
		return h, fmt.Errorf("unknown offer version %d, want %d", h.version, cubinHeaderVersion)
	}
	// Reserved means reserved. A producer that starts setting a flag this
	// consumer does not understand is telling it something about the payload,
	// and guessing is how a wrong line table gets built.
	if h.flags != 0 {
		return h, fmt.Errorf("unknown offer flags %#04x", h.flags)
	}
	if h.size == 0 {
		return h, errors.New("declared size is zero")
	}
	return h, nil
}

// encodeCubinHeader is the inverse, used by the tests' producer and by
// nothing in production. Kept beside the decoder so the two cannot drift.
func encodeCubinHeader(h cubinHeader) []byte {
	b := make([]byte, cubinHeaderSize)
	binary.LittleEndian.PutUint32(b[0:4], h.magic)
	binary.LittleEndian.PutUint16(b[4:6], h.version)
	binary.LittleEndian.PutUint16(b[6:8], h.flags)
	binary.LittleEndian.PutUint64(b[8:16], h.size)
	binary.LittleEndian.PutUint64(b[16:24], h.crc)
	return b
}

// cubinAddressFor is the one place the cubin rendezvous name is spelled.
//
// Deliberately the same SHAPE as enrollAddressFor - same version segment,
// same decimal major.minor.inode tail from the same enrollShimIdentity
// derivation - and deliberately a different prefix. The shape is what makes
// it inherit the btrfs fix (stat(2) reports a different device than
// /proc/<pid>/maps for every file on a btrfs subvolume, which is how the
// enrolment rendezvous silently bound one name while the producer dialled
// another); the different prefix is what makes it a different socket.
func cubinAddressFor(dev, ino uint64) string {
	return fmt.Sprintf("@perfagent-gpu-cubin.v1.%d.%d.%d",
		unix.Major(dev), unix.Minor(dev), ino)
}

// cubinAddress is the offer name for shimPath.
func cubinAddress(shimPath string) (string, error) {
	dev, ino, err := enrollShimIdentity(shimPath)
	if err != nil {
		return "", err
	}
	return cubinAddressFor(dev, ino), nil
}

// newCubinAdmission builds a token bucket of the same shape the enrolment
// listener uses, with the cubin channel's own numbers - and, crucially, as
// its OWN instance. Two buckets is the whole mechanism: an offer cannot spend
// an enrolment's token because there is no token they share.
func newCubinAdmission(now func() time.Time) *enrollAdmission {
	if now == nil {
		now = time.Now
	}
	return &enrollAdmission{
		now:      now,
		total:    cubinTotalBurst,
		totalAt:  now(),
		perUID:   map[uint32]*enrollBucket{},
		burst:    cubinTotalBurst,
		refill:   cubinTotalRefill,
		uidBurst: cubinUIDBurst,
		uidFill:  cubinUIDRefill,
	}
}

// cubinSink is what an accepted cubin is handed to. gpu.ModuleStore - bounded,
// LRU, line-table-backed - is the sink a consumer that resolves source lines
// installs, through Config.Modules and moduleStoreSink below. memCubinStore is
// what a consumer with no store gets, so the transport is complete and
// testable on its own.
type cubinSink interface {
	// HasCubin reports whether this CRC is already held. Asked BEFORE the
	// payload is touched, so a duplicate costs no mmap and no parse.
	HasCubin(crc uint64) bool
	// PutCubin takes ownership of bytes.
	PutCubin(crc uint64, bytes []byte) error
}

// moduleStoreSink is the one hop between the cubin transport and the store
// that turns (cubin_crc, functionIndex, pcOffset) into a function, a file and
// a line. It is the whole of issue #93: both ends were built and tested and
// nothing connected them, so every PC sample in a real profile read
// gpu_src_status="no-module".
//
// It is an adapter rather than a set of methods on gpu.ModuleStore because the
// two contracts differ in one place that matters - see PutCubin - and because
// gpu must not grow a method named for this transport.
//
// It holds the store the CALLER built (Config.Modules) and never one of its
// own. gpu.TimelineConfig.Modules and gpu.ProjectionConfig.Modules read the
// same instance: one store, three references, no second copy of the bytes. A
// store constructed here would be a store the projection cannot see, which is
// the shape of the bug this closes.
type moduleStoreSink struct {
	store *gpu.ModuleStore
}

// HasCubin asks the store for LIVE membership, which is what makes the
// duplicate no-op safe against eviction: a module the store's bounds dropped
// answers false, so the next offer for it is admitted and stores it again. No
// set of "CRCs seen once" is kept anywhere on this path, deliberately - one
// would turn a single eviction into permanent unresolvability while
// CubinsReceived, CubinsDuplicate and every store counter still read healthy.
func (m moduleStoreSink) HasCubin(crc uint64) bool { return m.store.Has(crc) }

// PutCubin stores the bytes and reports only a failure to STORE them.
//
// gpu.ModuleStore.Put's error is diagnostic, not a rejection: bytes that do
// not parse are still stored (so a re-offer is not re-parsed), counted in
// ModuleStoreStats.ModulesUnparseable, and resolve as no-module. Propagating
// it here would make the listener count a rejection for a cubin it is holding
// - CubinsRejectedMalformed non-zero on a healthy run, CubinsReceived and
// CubinBytesReceived understating what the agent actually holds, and the
// producer told 'X' for an offer that landed. The two facts stay where each is
// true: the transport counts what arrived, and the store counts what it can
// read. Put has no other error, so nothing is being swallowed - if it grows
// one, this returns it.
func (m moduleStoreSink) PutCubin(crc uint64, b []byte) error {
	_ = m.store.Put(crc, b)
	return nil
}

// cubinSinkFor picks the sink for a consumer's configuration. A store supplied
// by the caller is the sink; nothing is constructed here when one is missing,
// because a store this package owned would be one the projection cannot read.
func cubinSinkFor(cfg Config) cubinSink {
	if cfg.Modules != nil {
		return moduleStoreSink{store: cfg.Modules}
	}
	return newMemCubinStore(defaultCubinStoreCapacity)
}

// memCubinStore is what a consumer with no Config.Modules gets: bounded by
// count, no line table, no LRU, and nothing reads it. Its PC samples resolve
// gpu_src_status="no-module", which is the truth for them - there is no store
// to resolve against. See Config.Modules.
type memCubinStore struct {
	mu   sync.Mutex
	cap  int
	seen map[uint64][]byte
}

func newMemCubinStore(capacity int) *memCubinStore {
	if capacity <= 0 {
		capacity = defaultCubinStoreCapacity
	}
	return &memCubinStore{cap: capacity, seen: map[uint64][]byte{}}
}

func (s *memCubinStore) HasCubin(crc uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seen[crc]
	return ok
}

func (s *memCubinStore) PutCubin(crc uint64, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[crc]; ok {
		return nil
	}
	if len(s.seen) >= s.cap {
		return fmt.Errorf("cubin store full at %d modules", s.cap)
	}
	s.seen[crc] = b
	return nil
}

func (s *memCubinStore) get(crc uint64) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.seen[crc]
	return b, ok
}

// cubinStats is the listener's slice of gpuprobe.Stats, under its own lock.
// Every field is a counted outcome of one offer, and the rejection fields are
// exhaustive: an offer that is not counted in `received` or `duplicate` is
// counted in exactly one of the four rejections or in `throttled`.
type cubinStats struct {
	received     uint64
	bytes        uint64
	duplicate    uint64
	tooLarge     uint64
	malformed    uint64
	unsealed     uint64
	unauthorized uint64
	throttled    uint64
	lastErr      string

	// mapped counts mmap calls. Not a Stats field and not an operator
	// signal: it is the only way a test can assert that a rejected offer was
	// never mapped, which for the seal check is the entire property.
	mapped uint64
}

// cubinListener serves the cubin offer channel for one consumer. Structurally
// a sibling of enrollListener and deliberately not a subclass of it: the two
// share their authentication helpers and share nothing else, least of all a
// goroutine or a bucket.
type cubinListener struct {
	ln   *net.UnixListener
	addr string
	sink cubinSink
	// pid mirrors Config.PID, exactly as enrollListener.pid does: a per-PID
	// attach must not accept another process's modules, and the socket name
	// is not PID-scoped and cannot be.
	pid uint32
	// dev and ino identify the shim image a peer must have mapped.
	dev, ino uint64
	// procRoot is "/proc" in production; tests point it at a fixture.
	procRoot string
	// requireUID, when set, is the only peer uid this listener will serve.
	// Same rule and same derivation as the enrolment listener's.
	requireUID *uint32
	// admit is the CUBIN bucket. Touched only from serve().
	admit *enrollAdmission

	maxBytes   uint64
	totalBytes uint64

	wg sync.WaitGroup

	mu    sync.Mutex
	stats cubinStats
}

// newCubinListener binds the offer channel for cfg's shim and starts serving.
//
// Unlike the enrolment listener there is no ordering requirement against the
// uprobe link: an offer is not a rendezvous and no producer blocks on it. A
// module offered before this is listening is simply an unresolvable module,
// counted by the shim as a send failure and read here as gpu_src_status
// "no-module".
func newCubinListener(cfg Config, sink cubinSink) (*cubinListener, error) {
	l, err := buildCubinListener(cfg, sink)
	if err != nil {
		return nil, err
	}
	l.start()
	return l, nil
}

// buildCubinListener binds without serving, so a test can settle state that
// only serve() is allowed to touch afterwards - `admit` has no lock precisely
// because one goroutine reaches it.
func buildCubinListener(cfg Config, sink cubinSink) (*cubinListener, error) {
	// The same derivation as the enrolment listener's, for both the address
	// and the peer check, and for the same reason: a consumer that bound one
	// device and checked peers against another would refuse every genuine
	// producer.
	dev, ino, err := enrollShimIdentity(cfg.ShimPath)
	if err != nil {
		return nil, err
	}
	addr := cubinAddressFor(dev, ino)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", addr, err)
	}
	maxBytes := uint64(defaultCubinMaxBytes)
	if cfg.CubinMaxBytes > 0 {
		maxBytes = uint64(cfg.CubinMaxBytes)
	}
	totalBytes := uint64(defaultCubinTotalBytes)
	if cfg.CubinTotalBytes > 0 {
		totalBytes = uint64(cfg.CubinTotalBytes)
	}
	if sink == nil {
		sink = cubinSinkFor(cfg)
	}
	return &cubinListener{
		ln:         ln,
		addr:       addr,
		sink:       sink,
		pid:        uint32(cfg.PID),
		dev:        dev,
		ino:        ino,
		procRoot:   "/proc",
		admit:      newCubinAdmission(nil),
		requireUID: enrollRequiredUID(),
		maxBytes:   maxBytes,
		totalBytes: totalBytes,
	}, nil
}

func (l *cubinListener) start() {
	l.wg.Add(1)
	go l.serve()
}

// serve accepts one offer at a time.
//
// Serial for the same reason enrollListener.serve() is - the work behind it
// is a parse and a store insert, and N producers interleaving would only
// produce the same total - but the consequence is different and much smaller.
// Nothing blocks on this loop. A queue of offers delays MODULE RESOLUTION,
// never a producer's startup, and never an enrolment, because the enrolment
// listener has its own Accept loop in its own goroutine.
func (l *cubinListener) serve() {
	defer l.wg.Done()
	for {
		conn, err := l.ln.AcceptUnix()
		if err != nil {
			return
		}
		l.handle(conn)
	}
}

// handle takes one offer. Every path out of it is counted, and every path out
// of it closes the received descriptor.
//
// The order of the checks is the design. Cheapest and most hostile first:
// credentials, then the bucket (so an exhausted peer cannot spend our /proc
// time either), then identity, and only then anything that touches the
// payload. Within the payload, the SEALS are checked before the size and
// before the map, because until the seals verify nothing about the fd is
// stable enough to be worth reading - including its size.
func (l *cubinListener) handle(conn *net.UnixConn) {
	defer func() { _ = conn.Close() }()

	pid, uid, err := enrollPeerCreds(conn)
	if err != nil {
		l.reject(conn, &l.stats.unauthorized, "peer credentials: "+err.Error())
		return
	}
	if !l.admit.admit(uid) {
		l.mu.Lock()
		l.stats.throttled++
		l.stats.lastErr = fmt.Sprintf("uid %d throttled at pid %d; cubin offer rate exceeded", uid, pid)
		l.mu.Unlock()
		l.reply(conn, cubinReplyRefused)
		return
	}
	if l.requireUID != nil && uid != *l.requireUID {
		l.reject(conn, &l.stats.unauthorized,
			fmt.Sprintf("pid %d has uid %d; this consumer is unprivileged and serves only uid %d",
				pid, uid, *l.requireUID))
		return
	}
	if l.pid != 0 && pid != l.pid {
		l.reject(conn, &l.stats.unauthorized,
			fmt.Sprintf("pid %d (uid %d) is not the attached pid %d", pid, uid, l.pid))
		return
	}
	mapped, err := procMapsHaveInode(l.procRoot, pid, l.dev, l.ino)
	if err != nil {
		l.reject(conn, &l.stats.unauthorized, fmt.Sprintf("read maps for pid %d: %v", pid, err))
		return
	}
	if !mapped {
		l.reject(conn, &l.stats.unauthorized,
			fmt.Sprintf("pid %d (uid %d) does not map the shim (dev %d:%d ino %d)",
				pid, uid, unix.Major(l.dev), unix.Minor(l.dev), l.ino))
		return
	}

	_ = conn.SetDeadline(time.Now().Add(cubinIOTimeout))
	hdr, fd, err := recvCubinOffer(conn)
	if fd >= 0 {
		defer func() { _ = unix.Close(fd) }()
	}
	if err != nil {
		l.reject(conn, &l.stats.malformed, fmt.Sprintf("pid %d: %v", pid, err))
		return
	}

	// The seals, before anything is read from the descriptor and long before
	// anything is mapped. There is no fallback branch here on purpose: a
	// fallback that reads an unsealed fd anyway is how a defended path
	// becomes an undefended one, and the two things it defends against are
	// this process being SIGBUSed and this process parsing bytes that
	// changed underneath it.
	if err := verifyCubinSeals(fd); err != nil {
		l.reject(conn, &l.stats.unsealed, fmt.Sprintf("pid %d crc %#x: %v", pid, hdr.crc, err))
		return
	}

	// A CRC we already hold is a no-op, decided from the header alone: no
	// fstat, no mmap, no copy, no re-parse. cubin_crc is content-addressed,
	// so "the same CRC" is by definition the same bytes and re-reading them
	// could only produce the same answer at the cost of doing it again.
	if l.sink.HasCubin(hdr.crc) {
		l.mu.Lock()
		l.stats.duplicate++
		l.mu.Unlock()
		l.reply(conn, cubinReplyOK)
		return
	}

	if hdr.size > l.maxBytes {
		l.reject(conn, &l.stats.tooLarge,
			fmt.Sprintf("pid %d crc %#x: %d bytes over the per-cubin ceiling of %d",
				pid, hdr.crc, hdr.size, l.maxBytes))
		return
	}
	// The total ceiling shares tooLarge deliberately: from the reader's side
	// both mean "this cubin was refused for its size and nothing partial was
	// kept", and the reason string says which ceiling it was. What must never
	// happen for either is a truncated store - a truncated cubin parses into
	// a WRONG line table, which is the one failure worse than no line table.
	l.mu.Lock()
	held := l.stats.bytes
	l.mu.Unlock()
	if held+hdr.size > l.totalBytes {
		l.reject(conn, &l.stats.tooLarge,
			fmt.Sprintf("pid %d crc %#x: %d bytes would pass the total ceiling of %d (%d held)",
				pid, hdr.crc, hdr.size, l.totalBytes, held))
		return
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		l.reject(conn, &l.stats.malformed, fmt.Sprintf("pid %d crc %#x: fstat: %v", pid, hdr.crc, err))
		return
	}
	// The header is a claim and the file is the fact. They must agree
	// exactly: a payload shorter than the declared size would be a truncated
	// cubin, and a longer one means the two ends disagree about what was
	// offered, which is not a difference worth guessing about.
	if uint64(st.Size) != hdr.size {
		l.reject(conn, &l.stats.malformed,
			fmt.Sprintf("pid %d crc %#x: declared %d bytes, memfd holds %d",
				pid, hdr.crc, hdr.size, uint64(st.Size)))
		return
	}

	l.mu.Lock()
	l.stats.mapped++
	l.mu.Unlock()
	// MAP_PRIVATE and PROT_READ. The seals make the mapping stable; the
	// flags make it impossible for this end to be the one that changes it.
	m, err := unix.Mmap(fd, 0, int(hdr.size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		l.reject(conn, &l.stats.malformed, fmt.Sprintf("pid %d crc %#x: mmap: %v", pid, hdr.crc, err))
		return
	}
	// One copy out of the mapping, then the mapping goes away. The copy is
	// what the store owns; holding the mapping instead would tie every stored
	// module to a live descriptor for the life of the agent.
	owned := make([]byte, hdr.size)
	copy(owned, m)
	if err := unix.Munmap(m); err != nil {
		l.reject(conn, &l.stats.malformed, fmt.Sprintf("pid %d crc %#x: munmap: %v", pid, hdr.crc, err))
		return
	}

	if err := l.sink.PutCubin(hdr.crc, owned); err != nil {
		l.reject(conn, &l.stats.malformed, fmt.Sprintf("pid %d crc %#x: store: %v", pid, hdr.crc, err))
		return
	}
	l.mu.Lock()
	l.stats.received++
	l.stats.bytes += hdr.size
	l.mu.Unlock()
	l.reply(conn, cubinReplyOK)
}

// reject counts one refusal against the given field, records why, and
// releases the peer. The counter is passed in rather than switched on so that
// adding a rejection path without a counter does not compile into silence.
func (l *cubinListener) reject(conn *net.UnixConn, counter *uint64, reason string) {
	l.mu.Lock()
	*counter++
	l.stats.lastErr = reason
	l.mu.Unlock()
	l.reply(conn, cubinReplyRefused)
}

// reply hands the producer its status byte. It is advisory: the shim counts
// it so a run can say how many offers landed, and never waits on it beyond
// its own budget.
func (l *cubinListener) reply(conn *net.UnixConn, b byte) {
	_ = conn.SetWriteDeadline(time.Now().Add(cubinIOTimeout))
	_, _ = conn.Write([]byte{b})
}

func (l *cubinListener) snapshot() cubinStats {
	if l == nil {
		return cubinStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

func (l *cubinListener) address() string {
	if l == nil {
		return ""
	}
	return l.addr
}

func (l *cubinListener) close() error {
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

// verifyCubinSeals is the whole seal check, and it is all-or-nothing.
//
// F_GET_SEALS fails with EINVAL on a file that cannot be sealed at all, so
// this doubles as "is this actually a memfd" - a peer that passes a regular
// file, a pipe or a socket is refused here rather than somewhere later with a
// stranger message.
func verifyCubinSeals(fd int) error {
	seals, err := unix.FcntlInt(uintptr(fd), unix.F_GET_SEALS, 0)
	if err != nil {
		return fmt.Errorf("F_GET_SEALS: %w (not a sealable memfd?)", err)
	}
	if missing := cubinRequiredSeals &^ seals; missing != 0 {
		return fmt.Errorf("missing seals %s (have %#x, need %#x)",
			cubinSealNames(missing), seals, cubinRequiredSeals)
	}
	return nil
}

// cubinSealNames spells a seal mask so the rejection reason names the seal
// that was missing rather than a hex number nobody decodes twice.
func cubinSealNames(mask int) string {
	names := []struct {
		bit  int
		name string
	}{
		{unix.F_SEAL_SEAL, "F_SEAL_SEAL"},
		{unix.F_SEAL_SHRINK, "F_SEAL_SHRINK"},
		{unix.F_SEAL_GROW, "F_SEAL_GROW"},
		{unix.F_SEAL_WRITE, "F_SEAL_WRITE"},
	}
	out := ""
	for _, n := range names {
		if mask&n.bit == 0 {
			continue
		}
		if out != "" {
			out += "|"
		}
		out += n.name
	}
	if out == "" {
		return "none"
	}
	return out
}

// recvCubinOffer reads the fixed header and the one descriptor that must
// accompany it.
//
// The descriptor rides on the first byte of the header, so it arrives with
// the first ReadMsgUnix; the rest of the header is an ordinary stream read.
// Returned fd is -1 when none arrived, and the caller closes it on every
// path - including the error paths, which is why it is returned even when
// err is non-nil.
func recvCubinOffer(conn *net.UnixConn) (cubinHeader, int, error) {
	buf := make([]byte, cubinHeaderSize)
	// Room for more descriptors than a well-formed offer carries, on
	// purpose: with a buffer sized for exactly one, a peer sending two would
	// have the second silently discarded by the kernel (MSG_CTRUNC) and we
	// would never know it had tried. With room to see them, extra
	// descriptors are closed and the offer is refused as malformed.
	oob := make([]byte, unix.CmsgSpace(4*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return cubinHeader{}, -1, fmt.Errorf("read offer: %w", err)
	}
	fds := parseCubinRights(oob[:oobn])
	// Close everything past the first before any early return can skip it.
	for _, extra := range fds[min(len(fds), 1):] {
		_ = unix.Close(extra)
	}
	fd := -1
	if len(fds) > 0 {
		fd = fds[0]
	}
	if len(fds) != 1 {
		return cubinHeader{}, fd, fmt.Errorf("offer carried %d descriptors, want exactly 1", len(fds))
	}
	if n < cubinHeaderSize {
		if _, err := io.ReadFull(conn, buf[n:]); err != nil {
			return cubinHeader{}, fd, fmt.Errorf("read offer header: %w", err)
		}
	}
	h, err := decodeCubinHeader(buf)
	return h, fd, err
}

// parseCubinRights pulls every descriptor out of the control message,
// tolerating a malformed one rather than failing before the descriptors it
// did parse can be closed.
func parseCubinRights(oob []byte) []int {
	if len(oob) == 0 {
		return nil
	}
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var fds []int
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_SOCKET || m.Header.Type != unix.SCM_RIGHTS {
			continue
		}
		got, err := unix.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		fds = append(fds, got...)
	}
	return fds
}
