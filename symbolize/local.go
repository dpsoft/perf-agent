package symbolize

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	blazesym "github.com/libbpf/blazesym/go"
)

// ErrClosed is returned from operations on a closed Symbolizer.
var ErrClosed = errors.New("symbolize: closed")

// errSkippedMapFiles stands in for the first attempt's error once the
// map_files path has been latched off. It never escapes SymbolizeProcess.
var errSkippedMapFiles = errors.New("symbolize: map_files disabled")

// LocalSymbolizer wraps blazesym's Process source with no off-box hooks —
// preserves perf-agent's pre-debuginfod behavior. Used when no debuginfod
// URL is configured.
type LocalSymbolizer struct {
	bz     *blazesym.Symbolizer
	closed atomic.Bool
	// noMapFiles latches once /proc/<pid>/map_files/ has been proven
	// unusable for this process - either because the startup capability
	// probe said so, or because a symbolization actually failed with
	// permission denied (see SymbolizeProcess).
	//
	// It is NOT a plain performance latch, which is what it was first
	// written as, and what made it wrong: it downgrades every later
	// symbolization, for every process, to symbolic /proc/<pid>/maps paths,
	// which under overlayfs can be re-pointed between the mmap and the read
	// and then resolve to the WRONG symbols rather than to none. Paying that
	// for the life of the symbolizer because one pid vanished mid-batch, or
	// because a frame landed in a JIT region, is not a trade worth making
	// silently. So only a permission failure - the one cause that cannot
	// improve on its own - may set it.
	noMapFiles atomic.Bool
	stats      localCounters
	// mapFilesAttempt overrides the first, inode-accurate attempt. It is a
	// field only so a test can inject the two failure kinds SymbolizeProcess
	// has to tell apart - blazesym itself offers no way to ask for one -
	// while the retry underneath stays real. Nil in production.
	mapFilesAttempt func(ips []uint64, pid uint32, opts []blazesym.ProcessSourceOption) ([]blazesym.Sym, error)
}

// symbolizeMapFiles runs the first attempt, through the test seam if one is
// installed.
func (s *LocalSymbolizer) symbolizeMapFiles(ips []uint64, pid uint32, opts []blazesym.ProcessSourceOption) ([]blazesym.Sym, error) {
	if s.mapFilesAttempt != nil {
		return s.mapFilesAttempt(ips, pid, opts)
	}
	return s.bz.SymbolizeProcessAbsAddrs(ips, pid, opts...)
}

// localCounters are the process-side symbolization counters. Kept separate
// from Counters, which is the kernel symbolizer's and is already wired into
// the /metrics endpoint with a fixed field set.
type localCounters struct {
	mapFilesPermissionDenied atomic.Uint64
	mapFilesTransientFailure atomic.Uint64
	fallbackRescued          atomic.Uint64
	rawAddrBatches           atomic.Uint64
	disabledReason           atomic.Value // string
}

// LocalStats is a point-in-time view of what SymbolizeProcess has had to
// degrade to. The map_files transition used to be invisible - no counter, no
// log - which meant a profiler could silently spend an entire run resolving
// through re-pointable symbolic paths and nothing would say so.
type LocalStats struct {
	// MapFilesDisabled reports whether the inode-accurate
	// /proc/<pid>/map_files/ path has been latched off.
	MapFilesDisabled bool
	// MapFilesDisabledReason says why, and is empty while it is still on.
	MapFilesDisabledReason string
	// MapFilesPermissionDenied counts first attempts that failed with
	// permission denied. Only these latch.
	MapFilesPermissionDenied uint64
	// MapFilesTransientFailure counts first attempts that failed for any
	// other reason - a deleted mapping, a JIT region, a pid that exited
	// mid-batch. These deliberately do NOT latch: the next batch gets the
	// inode-accurate path back.
	MapFilesTransientFailure uint64
	// FallbackRescued counts batches the no_map_files retry saved.
	FallbackRescued uint64
	// RawAddrBatches counts batches where the retry failed too and every
	// frame came back as a bare hex address.
	RawAddrBatches uint64
}

// Stats returns the current process-side symbolization counters.
func (s *LocalSymbolizer) Stats() LocalStats {
	reason, _ := s.stats.disabledReason.Load().(string)
	return LocalStats{
		MapFilesDisabled:         s.noMapFiles.Load(),
		MapFilesDisabledReason:   reason,
		MapFilesPermissionDenied: s.stats.mapFilesPermissionDenied.Load(),
		MapFilesTransientFailure: s.stats.mapFilesTransientFailure.Load(),
		FallbackRescued:          s.stats.fallbackRescued.Load(),
		RawAddrBatches:           s.stats.rawAddrBatches.Load(),
	}
}

// disableMapFiles latches the fallback on and says so exactly once. The
// compare-and-swap is what makes it once: a second caller finds the latch
// already set and returns without logging again.
func (s *LocalSymbolizer) disableMapFiles(reason string) {
	if !s.noMapFiles.CompareAndSwap(false, true) {
		return
	}
	s.stats.disabledReason.Store(reason)
	log.Printf("symbolize: /proc/<pid>/map_files/ unusable (%s); "+
		"falling back to /proc/<pid>/maps symbolic paths for all "+
		"processes - symbols stay resolvable but are no longer "+
		"inode-accurate", reason)
}

// capCheckpointRestore and capSysAdmin are the two capabilities the kernel's
// proc_map_files_get_link() accepts; without either, following a map_files
// magic symlink is EPERM no matter which process is targeted.
const (
	capSysAdmin          = 21
	capCheckpointRestore = 40
)

// canFollowMapFiles reports whether this process holds a capability that lets
// it follow /proc/<pid>/map_files/ links. Probed once, at construction, so
// the common setcap'd case (cap_bpf,cap_perfmon and nothing else - the
// configuration that produced hex-named frames in every shipped profile/ and
// offcpu/ run) skips the doomed first attempt from the very first batch
// instead of discovering it by failing.
//
// An unreadable or unparsable /proc/self/status returns true: the cost of
// guessing "capable" wrongly is one failed attempt per batch until a real
// permission error latches it, whereas guessing "incapable" wrongly would
// give up inode accuracy that the process actually has.
func canFollowMapFiles() bool {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return true
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		hex, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		caps, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
		if err != nil {
			return true
		}
		return caps&(1<<capSysAdmin) != 0 || caps&(1<<capCheckpointRestore) != 0
	}
	return true
}

// isPermissionDenied reports whether err is the one failure that can never
// improve on its own.
//
// The typed check comes first and covers anything that carries an errno
// (syscall.EPERM and syscall.EACCES both satisfy errors.Is(_, os.ErrPermission)).
// blazesym is not that: its C API collapses the errno into an enum, blaze_err_str
// renders BLAZE_ERR_PERMISSION_DENIED as the bare string "permission denied",
// and the Go binding wraps that with errors.New - so by the time the error
// reaches here there is nothing typed left to match, and the string is the
// only evidence. Matching it is narrow and it is checked by a test; the
// alternative is latching on every failure, which is the bug being fixed.
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "permission denied")
}

// NewLocalSymbolizer constructs a LocalSymbolizer with code-info and
// inlined-fns enabled (matches today's behavior at the three call sites).
func NewLocalSymbolizer() (*LocalSymbolizer, error) {
	bz, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		return nil, err
	}
	s := &LocalSymbolizer{bz: bz}
	if !canFollowMapFiles() {
		s.disableMapFiles("no CAP_CHECKPOINT_RESTORE or CAP_SYS_ADMIN")
	}
	return s, nil
}

// SymbolizeProcess returns one Frame per IP. blazesym's Inlined chain is
// expanded into the Frame.Inlined slice in caller-most-to-callee order.
//
// blazesym's process source resolves each mapping through
// /proc/<pid>/map_files/<range>, which is inode-accurate: it cannot be
// fooled by a binary that was replaced or deleted since it was mapped.
// Following those magic symlinks is privileged — the kernel's
// proc_map_files_get_link() rejects the open with EPERM unless the caller
// holds CAP_CHECKPOINT_RESTORE (or CAP_SYS_ADMIN) — so on a perf-agent that
// was setcap'd with only cap_bpf,cap_perfmon the ENTIRE batch fails with
// "permission denied" and every frame degrades to a bare address. Nothing
// about that failure is visible in the returned error, because the fallback
// below deliberately does not propagate it.
//
// So: on any error, retry once with no_map_files, which reads the symbolic
// paths out of /proc/<pid>/maps instead. That is what every other profiler
// does and needs no capability at all; it is second choice only because a
// symbolic path can be re-pointed at different contents between the mmap and
// the read - under overlayfs that yields WRONG symbols, not merely
// unresolved ones.
//
// The retry always runs. What is conditional is the LATCH that turns the
// first attempt off for every later batch and every later process: that
// happens only when map_files is closed to us for good, which means either
// the startup capability probe said so (canFollowMapFiles) or a first
// attempt actually failed with permission denied. A transient failure - a
// mapping deleted between the walk and the read, a JIT region with no file
// behind it, a pid that exited mid-batch - is rescued by the retry and then
// forgotten, because the next batch may well be fine and inode accuracy is
// worth one doomed attempt to get back. Latching on any failure at all,
// which is what this used to do, permanently traded accuracy for one bad
// moment, silently. Both paths are counted, and the latch logs once (see
// Stats and disableMapFiles).
//
// If the retry fails too — the usual reason being that the process exited and
// /proc/<pid>/maps is gone, "entity not found" — returns raw hex-named Frames
// instead of dropping the batch. That preserves stack shape and addresses so
// operators can decode with addr2line and the pprof's user mapping still has
// somewhere to attach. Those frames carry Reason == FailureMissingSymbols;
// callers that need to know symbolization resolved nothing must inspect
// Frame.Reason, because err is nil on this path (see
// gpuprobe.Stats.StacksUnresolved for a consumer that does).
func (s *LocalSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(ips) == 0 {
		return nil, nil
	}
	opts := []blazesym.ProcessSourceOption{
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	}
	var syms []blazesym.Sym
	var err error
	skipped := s.noMapFiles.Load()
	if !skipped {
		syms, err = s.symbolizeMapFiles(ips, pid, opts)
	} else {
		err = errSkippedMapFiles
	}
	if err != nil {
		denied := !skipped && isPermissionDenied(err)
		if !skipped {
			if denied {
				s.stats.mapFilesPermissionDenied.Add(1)
			} else {
				s.stats.mapFilesTransientFailure.Add(1)
			}
		}
		// A fresh slice, not append(opts, ...): opts must not gain the
		// no_map_files option as a side effect if this function ever grows a
		// second use of it.
		retryOpts := make([]blazesym.ProcessSourceOption, 0, len(opts)+1)
		retryOpts = append(retryOpts, opts...)
		retryOpts = append(retryOpts, blazesym.ProcessSourceWithoutMapFiles(true))
		var retryErr error
		syms, retryErr = s.bz.SymbolizeProcessAbsAddrs(ips, pid, retryOpts...)
		if retryErr != nil {
			s.stats.rawAddrBatches.Add(1)
			return rawUserAddrFrames(ips), nil
		}
		if !skipped {
			s.stats.fallbackRescued.Add(1)
		}
		// Only a permission failure means map_files will still be closed on
		// the next batch. Anything else gets the inode-accurate path back.
		if denied {
			s.disableMapFiles("blazesym reported permission denied")
		}
	}
	out := make([]Frame, 0, len(syms))
	for i, sym := range syms {
		var addr uint64
		if i < len(ips) {
			addr = ips[i]
		}
		out = append(out, fromBlazesymSym(sym, addr))
	}
	return out, nil
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
