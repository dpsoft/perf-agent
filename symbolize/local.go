package symbolize

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// ErrClosed is returned from operations on a closed Symbolizer.
var ErrClosed = errors.New("symbolize: closed")

// ErrMapFilesUnavailable is returned from NewLocalSymbolizer when this process
// cannot follow /proc/<pid>/map_files/ magic symlinks. Wrapped, so callers can
// test for it with errors.Is.
var ErrMapFilesUnavailable = errors.New("symbolize: cannot follow /proc/<pid>/map_files/")

// LocalSymbolizer wraps blazesym's Process source with no off-box hooks —
// preserves perf-agent's pre-debuginfod behavior. Used when no debuginfod
// URL is configured.
type LocalSymbolizer struct {
	bz      *blazesym.Symbolizer
	modules ModuleIndex
	// batchErrs deduplicates the batch-failure log by message.
	batchErrs sync.Map
	closed    atomic.Bool
	stats     localCounters
}

// LocalOption configures a LocalSymbolizer.
type LocalOption func(*LocalSymbolizer)

// WithModuleIndex supplies the /proc/<pid>/maps index used to name the module
// behind an address blazesym could not resolve to a symbol. Without it, an
// unresolved frame stays a bare hex address - which is the honest result, not
// a degraded one, because there is then nothing to say about it.
//
// Pass the *procmap.Resolver the rest of the pipeline already owns rather
// than building a private one: a second cache doubles the /proc parsing and
// gives the caller two things to keep fresh instead of one.
//
// The index is consulted only for frames blazesym failed on, and only with
// Lookup - a miss leaves the frame bare rather than guessing at a neighbour.
// What it cannot defend against is a Resolver whose cache has gone stale
// because the PID exited and was reused; keeping the cache honest is the
// owner's job, exactly as it already is for the Resolver the pprof builder
// uses to attribute every user frame.
func WithModuleIndex(idx ModuleIndex) LocalOption {
	return func(s *LocalSymbolizer) { s.modules = idx }
}

// localCounters are the process-side symbolization counters. Kept separate
// from Counters, which is the kernel symbolizer's and is already wired into
// the /metrics endpoint with a fixed field set.
type localCounters struct {
	rawAddrBatches  atomic.Uint64
	modulesAttached atomic.Uint64
	modulesBare     atomic.Uint64

	// The module-file retry. Named counts frames the process source could not
	// name and the file source could: a disagreement between two views of one
	// address, so zero is the healthy value and non-zero is the size of what
	// the process source was dropping.
	fileRetryNamed  atomic.Uint64
	fileRetryFailed atomic.Uint64

	// How much wall time symbolization actually cost, and over how many
	// calls. A profiler that takes two minutes to write a five-second capture
	// is a real failure mode (issue #109) and it was not attributable from
	// anything the agent printed: every other counter looked healthy while the
	// process sat in blazesym. These make the collect path's cost a number.
	calls   atomic.Uint64
	totalNs atomic.Uint64
	// The single most expensive call, with the PID that caused it. A mean
	// hides the case this is usually about --- one enormous binary parsed
	// once --- so the maximum is kept alongside the total rather than instead
	// of it.
	slowestNs  atomic.Uint64
	slowestPID atomic.Uint32
}

// LocalStats is a point-in-time view of what SymbolizeProcess could not
// resolve.
type LocalStats struct {
	// RawAddrBatches counts batches blazesym refused outright — the usual
	// cause being a pid that exited before its /proc entry could be read —
	// and where every frame therefore came back as a bare hex address.
	RawAddrBatches uint64
	// ModulesAttached counts frames that blazesym could not name but which
	// a mapping lookup placed inside a known file: they render as
	// "libcuda.so.1+0x1b71c6" rather than as a bare address.
	ModulesAttached uint64
	// ModulesBare counts frames that blazesym could not name and for which
	// no mapping was found either - no ModuleIndex configured, the process
	// already gone, or a PC genuinely outside every file-backed executable
	// range. These stay bare hex, and they are deliberately counted apart
	// from ModulesAttached: recovering the module is a real improvement and
	// a run where it never happens must not look like one where it always
	// did.
	ModulesBare uint64

	// FileRetryNamed counts frames the process source could not name and the
	// file source then could, for the same address. Zero is the healthy value.
	FileRetryNamed uint64
	// FileRetryFailed counts modules the retry could not ask about at all.
	// Kept apart from Named so "recovered nothing" and "could not look" stay
	// different facts.
	FileRetryFailed uint64

	// Calls is how many times SymbolizeProcess ran, and TotalNs the wall time
	// inside it. Calls far exceeding the number of distinct processes means
	// the caller is symbolizing per sample rather than per process.
	Calls   uint64
	TotalNs uint64
	// SlowestNs is the most expensive single call and SlowestPID the process
	// it was for. First contact with a large binary is normally the whole of
	// it: blazesym caches parsed sources, so the second call for the same
	// process costs almost nothing.
	SlowestNs  uint64
	SlowestPID uint32
}

// noteCall records one SymbolizeProcess call's cost. Racy for slowestPID
// against slowestNs under concurrent callers --- two calls can interleave and
// leave the PID of one with the duration of another. That is accepted: this is
// a diagnostic pointing at which process is expensive, not a measurement
// anything decides on, and paying a mutex per symbolization to tighten it
// would be spending the very thing being measured.
func (s *LocalSymbolizer) noteCall(pid uint32, d time.Duration) {
	ns := uint64(d.Nanoseconds())
	s.stats.calls.Add(1)
	s.stats.totalNs.Add(ns)
	for {
		prev := s.stats.slowestNs.Load()
		if ns <= prev {
			return
		}
		if s.stats.slowestNs.CompareAndSwap(prev, ns) {
			s.stats.slowestPID.Store(pid)
			return
		}
	}
}

// Stats returns the current process-side symbolization counters.
func (s *LocalSymbolizer) Stats() LocalStats {
	return LocalStats{
		RawAddrBatches:  s.stats.rawAddrBatches.Load(),
		ModulesAttached: s.stats.modulesAttached.Load(),
		ModulesBare:     s.stats.modulesBare.Load(),
		FileRetryNamed:  s.stats.fileRetryNamed.Load(),
		FileRetryFailed: s.stats.fileRetryFailed.Load(),
		Calls:           s.stats.calls.Load(),
		TotalNs:         s.stats.totalNs.Load(),
		SlowestNs:       s.stats.slowestNs.Load(),
		SlowestPID:      s.stats.slowestPID.Load(),
	}
}

// checkMapFilesAccess reports whether this process may follow a
// /proc/<pid>/map_files/ magic symlink, which is the single path blazesym's
// process source uses to reach the file behind a mapping. The kernel's
// proc_map_files_get_link() rejects the open with EPERM unless the caller
// holds CAP_CHECKPOINT_RESTORE (or CAP_SYS_ADMIN); perf-agent documents both
// in its required set, and unwind/procmap, unwind/dwarfagent and
// symbolize/debuginfod already depend on it directly.
//
// The check is an actual open() of a real map_files entry, not a read of
// CapEff out of /proc/self/status. It exercises the exact kernel gate that
// symbolization will hit, so it cannot be wrong about which capability the
// running kernel consults, about a capability held only in a non-initial user
// namespace (checkpoint_restore_ns_capable() checks &init_user_ns), or about
// Permitted-but-not-Effective. It also yields a typed errno, which is what
// lets this refuse without string-matching blazesym's error text.
//
// Only a definite EPERM is a verdict. An unreadable /proc/self/maps, a
// process with no file-backed mapping, or a mapping that was unmapped between
// the read and the open returns nil: a probe that could not decide must never
// be the reason a profiler refuses to start.
func checkMapFilesAccess() error {
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || !strings.HasPrefix(fields[5], "/") {
			continue
		}
		var start, end uint64
		if _, err := fmt.Sscanf(fields[0], "%x-%x", &start, &end); err != nil {
			continue
		}
		// Reformatted with %x rather than reused from /proc/self/maps: maps
		// pads each address to the word width, and the kernel's
		// dname_to_vma_addr() rejects a leading zero with -EINVAL, which
		// would surface as ENOENT and be mistaken for a vanished mapping.
		name := fmt.Sprintf("/proc/self/map_files/%x-%x", start, end)
		fh, err := os.Open(name)
		if err == nil {
			_ = fh.Close()
			return nil
		}
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w (%v): every user-space frame would resolve to a bare "+
				"hex address. Grant CAP_CHECKPOINT_RESTORE - "+
				"sudo setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep <binary> - "+
				"or run as root", ErrMapFilesUnavailable, err)
		}
		// Any other error is not an answer about capabilities: the mapping
		// was unmapped between the read and the open. Try the next one.
	}
	return nil
}

// NewLocalSymbolizer constructs a LocalSymbolizer with code-info and
// inlined-fns enabled (matches today's behavior at the three call sites).
//
// It refuses, rather than degrading, when /proc/<pid>/map_files/ is closed to
// this process. That is a deliberate trade. There used to be a second
// resolution path here - a retry with blazesym's no_map_files option, which
// reads the symbolic paths out of /proc/<pid>/maps and needs no capability -
// plus a latch, a once-only log, a startup CapEff probe and a string match on
// blazesym's "permission denied" text to drive them. All of it existed to
// tolerate one unsupported configuration: a binary setcap'd with only
// cap_bpf,cap_perfmon, which is narrower than the set perf-agent's own README,
// SECURITY.md and agent.go document as required.
//
// Keeping map_files as the single path costs nothing a supported deployment
// has, and buys two things. Inode accuracy: a symbolic path out of
// /proc/<pid>/maps can be re-pointed at different contents between the mmap
// and the read - under overlayfs that resolves to the WRONG symbols rather
// than to none - while a map_files link cannot. And a failure that is
// impossible to miss: the alternative considered here was a prominent
// one-time log plus a flag on LocalStats, which is silent degradation wearing
// a hat, because no caller in this repository reads LocalStats and a log line
// emitted at startup is gone by the time a sixty-second run writes a
// profile.pb.gz full of hex. A profiler whose every user frame is "0x7f..."
// is not degraded, it is useless, and this codebase refuses everywhere else
// rather than hand back output that looks like a result.
func NewLocalSymbolizer(opts ...LocalOption) (*LocalSymbolizer, error) {
	if err := checkMapFilesAccess(); err != nil {
		return nil, err
	}
	bz, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		return nil, err
	}
	s := &LocalSymbolizer{bz: bz}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SymbolizeProcess returns one Frame per IP. blazesym's Inlined chain is
// expanded into the Frame.Inlined slice in caller-most-to-callee order.
//
// If blazesym fails the batch — with the capability check in
// NewLocalSymbolizer standing, the realistic reason is that the process
// exited and /proc/<pid>/maps is gone, "entity not found" — returns raw
// hex-named Frames instead of dropping the batch. That preserves stack shape
// and addresses so operators can decode with addr2line and the pprof's user
// mapping still has somewhere to attach. Those frames carry Reason ==
// FailureMissingSymbols; callers that need to know symbolization resolved
// nothing must inspect Frame.Reason, because err is nil on this path (see
// gpuprobe.Stats.StacksUnresolved for a consumer that does). The batch is
// counted in LocalStats.RawAddrBatches: a per-process failure is real signal
// about that process and must not be silent.
func (s *LocalSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(ips) == 0 {
		return nil, nil
	}
	start := time.Now()
	syms, err := s.bz.SymbolizeProcessAbsAddrs(ips, pid,
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	)
	s.noteCall(pid, time.Since(start))
	if err != nil {
		// The whole batch failed. Every frame becomes a raw address, and
		// withModules then places each one in a file -- so the module and the
		// mapping ARE known here even though no name is.
		//
		// That is exactly the input the file-source retry needs, and this is
		// where it matters most: measured on a PyTorch capture, a single
		// failed batch turned 35 of 35 frames into module+offset, 17 of them
		// in libtorch, whose symbols are present on disk and recoverable.
		// Before this, the retry ran only on the success path, so the frames
		// that needed it most were the only ones it never saw.
		s.stats.rawAddrBatches.Add(1)
		frames := s.withModules(pid, rawUserAddrFrames(ips))
		s.retryAgainstModuleFiles(frames)
		s.noteBatchFailure(pid, err)
		return frames, nil
	}
	out := make([]Frame, 0, len(syms))
	for i, sym := range syms {
		var addr uint64
		if i < len(ips) {
			addr = ips[i]
		}
		out = append(out, fromBlazesymSym(sym, addr))
	}
	frames := s.withModules(pid, out)
	s.retryAgainstModuleFiles(frames)
	return frames, nil
}

// noteBatchFailure records why a batch failed, once per distinct message.
//
// The batch-failure path used to be silent apart from a counter nothing
// printed, and its doc comment asserted the realistic cause was the process
// having exited. That assertion went unchecked for as long as the counter went
// unread: in the capture that motivated this, batches failed while the target
// was demonstrably alive and still launching kernels. Keeping the reason makes
// the next such claim falsifiable.
func (s *LocalSymbolizer) noteBatchFailure(pid uint32, err error) {
	msg := err.Error()
	if _, seen := s.batchErrs.LoadOrStore(msg, struct{}{}); seen {
		return
	}
	log.Printf("symbolize: batch failed for pid %d: %v (frames fall back to module+offset; "+
		"names recovered from module files where possible)", pid, msg)
}

// retryAgainstModuleFiles re-asks blazesym for the frames the PROCESS source
// could not name, this time against the module file at a file offset.
//
// The two sources disagree, and the file source is the one that is right.
// Measured, for addresses the process source returned missing_symbols for:
//
//	0x28f4874 -> at::_ops::add_Tensor::redispatch(...)
//	0x5119b46 -> torch::autograd::VariableType::(anonymous namespace)::add_Tensor(...)
//
// Same library, same addresses, same instant. The second is a .symtab-only
// symbol which a dynamic-symbol view cannot see; the first is inside an
// exported .dynsym symbol and should have resolved either way, so "the process
// source only reads .dynsym" does not fully explain it and is not claimed
// here. What is established is the disagreement and its direction.
//
// Only frames that already carry a module AND a mapping are retried:
// MapStart/MapOff are what make a file offset computable, and a bare address
// has nothing to ask about. Frames are grouped by module, so a stack N frames
// deep into one library costs one call rather than N -- symbolization cost is
// not a free variable here (issue #109).
//
// Failure is not an error: a frame neither source can name keeps the
// module+offset rendering it already had. This can only add names.
func (s *LocalSymbolizer) retryAgainstModuleFiles(frames []Frame) {
	var byModule map[string][]int
	for i := range frames {
		f := &frames[i]
		if f.Reason == FailureNone || f.Module == "" || f.MapStart == 0 {
			continue
		}
		if strings.HasPrefix(f.Module, "[") {
			continue // [kernel], [jit] and friends are not files
		}
		if byModule == nil {
			byModule = make(map[string][]int, 4)
		}
		byModule[f.Module] = append(byModule[f.Module], i)
	}
	for mod, idxs := range byModule {
		// Address - MapStart + MapOff is a FILE offset; blazesym wants a
		// VIRTUAL one. The two coincide only when a segment's p_vaddr equals
		// its p_offset -- true of the CUDA and libtorch shared objects, and
		// false of, say, a non-PIE executable, where the difference is the
		// whole load address. Getting this wrong does not fail loudly: it
		// asks about a valid-looking offset and returns a confidently wrong
		// name. procmap.AddressMapper reads the program headers and does the
		// conversion properly; symbolize/debuginfod already relies on it.
		mapper, merr := procmap.NewAddressMapper(mod)
		if merr != nil {
			s.stats.fileRetryFailed.Add(1)
			continue
		}
		offs := make([]uint64, 0, len(idxs))
		keep := make([]int, 0, len(idxs))
		for _, i := range idxs {
			fileOff := frames[i].Address - frames[i].MapStart + frames[i].MapOff
			va, ok := mapper.FileOffsetToVirtualAddress(fileOff)
			if !ok {
				continue
			}
			offs = append(offs, va)
			keep = append(keep, i)
		}
		if len(offs) == 0 {
			continue
		}
		idxs = keep
		syms, err := s.bz.SymbolizeElfVirtOffsets(offs, mod, blazesym.ElfSourceWithDebugSyms(true))
		if err != nil || len(syms) != len(idxs) {
			s.stats.fileRetryFailed.Add(1)
			continue
		}
		for j, i := range idxs {
			if syms[j].Name == "" {
				continue
			}
			f := &frames[i]
			f.Name = syms[j].Name
			f.Offset = offs[j]
			f.Reason = FailureNone
			s.stats.fileRetryNamed.Add(1)
		}
	}
}

// withModules names the module behind every frame blazesym left unresolved,
// and counts what it could and could not place. Called on both return paths
// of SymbolizeProcess: the whole-batch failure produces exactly the frames
// that need this most.
func (s *LocalSymbolizer) withModules(pid uint32, frames []Frame) []Frame {
	attached, bare := attachModules(s.modules, pid, frames)
	if attached > 0 {
		s.stats.modulesAttached.Add(uint64(attached))
	}
	if bare > 0 {
		s.stats.modulesBare.Add(uint64(bare))
	}
	return frames
}

// Close releases the underlying blazesym Symbolizer. Idempotent.
func (s *LocalSymbolizer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	s.bz.Close()
	return nil
}

// fromBlazesymSym translates one blazesym.Sym into a Frame, populating
// Inlined in caller-most-to-callee order. addr is the abs IP this frame
// was resolved from.
//
// On per-IP miss (s.Name == "" — blazesym opened the binary but
// couldn't map this address to a symbol), Name is filled with the
// hex IP so the pprof Location renders as "0x<addr>" instead of
// "<unknown>". Symmetric to the kernel-side rawKernelAddrFrames /
// frameFromKernelCSym behavior — operators can decode with
// addr2line; <unknown> just hides the structure.
func fromBlazesymSym(s blazesym.Sym, addr uint64) Frame {
	name := s.Name
	reason := FailureNone
	if name == "" {
		name = fmt.Sprintf("0x%x", addr)
		reason = FailureMissingSymbols
	}
	f := Frame{
		Address: addr,
		Name:    name,
		Module:  s.Module,
		Offset:  s.Offset,
		Reason:  reason,
	}
	if s.CodeInfo != nil {
		f.File = s.CodeInfo.File
		f.Line = int(s.CodeInfo.Line)
		f.Column = int(s.CodeInfo.Column)
	}
	for _, in := range s.Inlined {
		inFrame := Frame{
			Address: addr,
			Name:    in.Name,
			Module:  s.Module,
		}
		if in.CodeInfo != nil {
			inFrame.File = in.CodeInfo.File
			inFrame.Line = int(in.CodeInfo.Line)
			inFrame.Column = int(in.CodeInfo.Column)
		}
		f.Inlined = append(f.Inlined, inFrame)
	}
	return f
}

// LogSymbolizationCost prints what symbolization cost this collect, when the
// symbolizer in use keeps that figure.
//
// It exists because the cost is invisible otherwise. A capture whose window
// was five seconds can spend two minutes here (issue #109), and until this
// there was nothing in the agent's output to say so: the sample count, the
// mapping count and every drop counter all read healthy while the process sat
// inside blazesym. Printed unconditionally rather than behind a threshold,
// because "symbolization took 40ms" is the line that makes "symbolization took
// 107s" legible when someone eventually sees it.
//
// prefix names the profiler, since a combined run has more than one.
func LogSymbolizationCost(sym Symbolizer, prefix string) {
	st, ok := sym.(interface{ Stats() LocalStats })
	if !ok {
		return
	}
	s := st.Stats()
	if s.Calls == 0 {
		return
	}
	log.Printf("%s: symbolization took %v over %d call(s); slowest single call %v (pid %d); "+
		"raw_addr_batches=%d modules_attached=%d modules_bare=%d",
		prefix, time.Duration(s.TotalNs).Round(time.Millisecond), s.Calls,
		time.Duration(s.SlowestNs).Round(time.Millisecond), s.SlowestPID,
		s.RawAddrBatches, s.ModulesAttached, s.ModulesBare)
}
