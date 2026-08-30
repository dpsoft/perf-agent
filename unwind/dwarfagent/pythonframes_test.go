package dwarfagent

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/dpsoft/perf-agent/symbolize"
)

// ----- Issue #83 / ruling T7-R5 on the DWARF path.
//
// walk_step's interpreter arm is shared by perf_dwarf, offcpu_dwarf and
// gpu_usdt, so a Python frame reaches this consumer exactly as it reaches
// gpuprobe's: two consecutive pcs[] slots holding a code-object address and
// an encoded instruction word, both tagged frameTagPython. Neither is an
// instruction pointer.

// encodeSample lays out a struct sample_record the way the BPF program does,
// including the tags trailer. Slots past n_pcs are poisoned in both arrays,
// because the BPF side copies both whole out of a reused per-CPU scratch
// buffer and the tail belongs to whatever that CPU sampled last.
func encodeSample(pcs []uint64, tags []uint8) []byte {
	buf := make([]byte, SampleRecordBytes)
	buf[25] = byte(len(pcs))
	for i := range MaxFrames {
		off := SampleHeaderBytes + i*8
		if i < len(pcs) {
			binary.LittleEndian.PutUint64(buf[off:off+8], pcs[i])
		} else {
			binary.LittleEndian.PutUint64(buf[off:off+8], 0xdeadbeefdeadbeef)
		}
		if i < len(tags) {
			buf[SampleHeaderBytes+MaxFrames*8+i] = tags[i]
		} else {
			buf[SampleHeaderBytes+MaxFrames*8+i] = frameTagNative
		}
	}
	return buf
}

func TestParseSampleDecodesTags(t *testing.T) {
	s, err := parseSample(encodeSample(
		[]uint64{0x401000, 0xc0de0000, 0x5a5a5a5a},
		[]uint8{frameTagNative, frameTagPython, frameTagPython},
	))
	if err != nil {
		t.Fatalf("parseSample: %v", err)
	}
	if len(s.Tags) != len(s.PCs) {
		t.Fatalf("len(Tags) = %d, len(PCs) = %d: they must be parallel", len(s.Tags), len(s.PCs))
	}
	want := []uint8{frameTagNative, frameTagPython, frameTagPython}
	for i := range want {
		if s.Tags[i] != want[i] {
			t.Errorf("Tags[%d] = %d, want %d", i, s.Tags[i], want[i])
		}
	}
}

// The tags array is fixed-size and copied whole out of a reused per-CPU
// scratch buffer, so bytes past n_pcs belong to an earlier sample. Reading
// one would fold two real native frames into an invented Python one.
func TestParseSampleDoesNotReadTagsPastNPcs(t *testing.T) {
	buf := encodeSample([]uint64{0x401000, 0x401100}, nil)
	for i := 2; i < MaxFrames; i++ {
		buf[SampleHeaderBytes+MaxFrames*8+i] = frameTagPython
	}
	s, err := parseSample(buf)
	if err != nil {
		t.Fatalf("parseSample: %v", err)
	}
	if len(s.Tags) != 2 {
		t.Fatalf("len(Tags) = %d, want 2 (only n_pcs tags are meaningful)", len(s.Tags))
	}
	slots, truncated := splitFrameSlots(s.PCs, s.Tags)
	if truncated {
		t.Error("a stale tag was read and turned the last native frame into half a Python pair")
	}
	if len(slots) != 2 || slots[0].python || slots[1].python {
		t.Errorf("stale tags leaked into the decode: %+v", slots)
	}
}

// A record from before the tags trailer existed (or one truncated before it)
// must read as all-native, never as Python. The failure direction matters:
// under-reporting Python frames loses information, inventing one corrupts a
// call path.
func TestMissingTagsReadAsNative(t *testing.T) {
	slots, truncated := splitFrameSlots([]uint64{1, 2, 3}, nil)
	if truncated {
		t.Error("no tags at all must not read as a truncated pair")
	}
	if len(slots) != 3 {
		t.Fatalf("len(slots) = %d, want 3", len(slots))
	}
	for i, sl := range slots {
		if sl.python {
			t.Errorf("slot %d decoded as Python with no tags present", i)
		}
	}
}

func TestSplitFrameSlotsFoldsPairsAndKeepsOrder(t *testing.T) {
	slots, truncated := splitFrameSlots(
		[]uint64{0x401000, 0xc0de0000, 0x5a5a5a5a, 0x401100},
		[]uint8{frameTagNative, frameTagPython, frameTagPython, frameTagNative},
	)
	if truncated {
		t.Fatal("a complete pair reported as truncated")
	}
	if len(slots) != 3 {
		t.Fatalf("len(slots) = %d, want 3 (two slots fold into one frame)", len(slots))
	}
	if slots[0].python || slots[2].python {
		t.Error("native slots decoded as Python")
	}
	if !slots[1].python || slots[1].pc != 0xc0de0000 || slots[1].instr != 0x5a5a5a5a {
		t.Errorf("Python frame decoded wrong: %+v", slots[1])
	}
}

func TestSplitFrameSlotsDropsATruncatedPair(t *testing.T) {
	slots, truncated := splitFrameSlots(
		[]uint64{0x401000, 0xc0de0000},
		[]uint8{frameTagNative, frameTagPython},
	)
	if !truncated {
		t.Error("a code object with no instruction word must be reported, not swallowed")
	}
	if len(slots) != 1 {
		t.Fatalf("len(slots) = %d, want 1: the half pair is dropped, not half-read", len(slots))
	}
}

// splitFrameSlots is called from more than one place per sample. A counter
// inside it would report a number that depends on how many callers exist.
func TestSplitFrameSlotsIsPure(t *testing.T) {
	before := PythonFrameCounters()
	for range 5 {
		splitFrameSlots([]uint64{0x401000, 0xc0de0000}, []uint8{frameTagNative, frameTagPython})
	}
	if after := PythonFrameCounters(); after != before {
		t.Errorf("splitFrameSlots moved a counter: before %+v after %+v", before, after)
	}
}

// countingSymbolizer records what it was asked to resolve.
type countingSymbolizer struct {
	got   [][]uint64
	frame func(ip uint64) symbolize.Frame
	short bool // return one fewer frame than IPs
	err   error
}

func (c *countingSymbolizer) Close() error { return nil }

func (c *countingSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	c.got = append(c.got, append([]uint64(nil), ips...))
	if c.err != nil {
		return nil, c.err
	}
	out := make([]symbolize.Frame, 0, len(ips))
	for _, ip := range ips {
		if c.frame != nil {
			out = append(out, c.frame(ip))
			continue
		}
		out = append(out, symbolize.Frame{Address: ip, Name: fmt.Sprintf("fn_%x", ip)})
	}
	if c.short && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, nil
}

// The core of the seam. A code object is not code: blazesym will place it in
// whatever mapping it falls in and hand back a frame, so the stack gains two
// plausible, wrong native frames and nothing says so.
func TestPythonSlotsNeverReachTheSymbolizer(t *testing.T) {
	sym := &countingSymbolizer{}
	slots, _ := splitFrameSlots(
		[]uint64{0x401000, 0xc0de0000, 0x5a5a5a5a, 0x401100},
		[]uint8{frameTagNative, frameTagPython, frameTagPython, frameTagNative},
	)

	frames := symbolizePID(sym, 4242, slots)

	if len(sym.got) != 1 {
		t.Fatalf("symbolizer called %d times, want 1", len(sym.got))
	}
	want := []uint64{0x401000, 0x401100}
	if len(sym.got[0]) != len(want) {
		t.Fatalf("symbolizer got %v, want only the native PCs %v", sym.got[0], want)
	}
	for i := range want {
		if sym.got[0][i] != want[i] {
			t.Fatalf("symbolizer got %v, want only the native PCs %v", sym.got[0], want)
		}
	}

	// And the Python frame keeps its position between its native neighbours.
	if len(frames) != 3 {
		t.Fatalf("len(frames) = %d, want 3", len(frames))
	}
	if frames[1].Name != pythonFrameName(0xc0de0000) {
		t.Errorf("frames[1].Name = %q, want the Python frame between its native caller and callee", frames[1].Name)
	}
	if !frames[1].Unresolved {
		t.Error("an unsymbolized Python frame must not read as a function genuinely named python:0x...")
	}
	if frames[0].Name != "fn_401000" || frames[2].Name != "fn_401100" {
		t.Errorf("native frames moved or were renamed: %q, %q", frames[0].Name, frames[2].Name)
	}
}

// Behaviour preserved from before this path learned about tags: a symbolizer
// that fails yields "[unknown]" placeholders carrying the raw PC, one per
// native slot, in order.
func TestSymbolizerFailureStillPlacesEveryFrame(t *testing.T) {
	sym := &countingSymbolizer{err: fmt.Errorf("no such process")}
	slots, _ := splitFrameSlots(
		[]uint64{0x401000, 0xc0de0000, 0x5a5a5a5a},
		[]uint8{frameTagNative, frameTagPython, frameTagPython},
	)

	frames := symbolizePID(sym, 4242, slots)
	if len(frames) != 2 {
		t.Fatalf("len(frames) = %d, want 2", len(frames))
	}
	if frames[0].Name != "[unknown]" || frames[0].Address != 0x401000 {
		t.Errorf("native placeholder wrong: %+v", frames[0])
	}
	if frames[1].Name != pythonFrameName(0xc0de0000) {
		t.Error("a failed native symbolization must not take the Python frame down with it")
	}
}

// symbolize/local.go promises one Frame per IP and the splice indexes on that
// promise. A short return must not silently drop the tail natives.
func TestAShortSymbolizerReturnIsCountedNotSpliced(t *testing.T) {
	before := PythonFrameCounters().SymbolizerCountMismatch
	sym := &countingSymbolizer{short: true}
	slots, _ := splitFrameSlots([]uint64{0x401000, 0x401100, 0x401200}, nil)

	frames := symbolizePID(sym, 4242, slots)
	if len(frames) != 3 {
		t.Fatalf("len(frames) = %d, want 3: every slot keeps its place", len(frames))
	}
	for i, f := range frames {
		if f.Name != "[unknown]" {
			t.Errorf("frames[%d].Name = %q, want the placeholder rendering", i, f.Name)
		}
	}
	if got := PythonFrameCounters().SymbolizerCountMismatch; got != before+1 {
		t.Errorf("SymbolizerCountMismatch = %d, want %d: a short return must not be silent", got, before+1)
	}
}

// An inline chain expands to several pprof frames for one slot. The splice
// runs before that expansion precisely so a Python frame lands beside the
// slot it belongs to rather than at an index the expansion has moved.
func TestPythonFramesSurviveInlineExpansion(t *testing.T) {
	sym := &countingSymbolizer{frame: func(ip uint64) symbolize.Frame {
		return symbolize.Frame{
			Address: ip,
			Name:    fmt.Sprintf("fn_%x", ip),
			Inlined: []symbolize.Frame{{Name: fmt.Sprintf("inl_%x", ip)}},
		}
	}}
	slots, _ := splitFrameSlots(
		[]uint64{0x401000, 0xc0de0000, 0x5a5a5a5a, 0x401100},
		[]uint8{frameTagNative, frameTagPython, frameTagPython, frameTagNative},
	)

	frames := symbolizePID(sym, 4242, slots)
	names := make([]string, len(frames))
	for i, f := range frames {
		names[i] = f.Name
	}
	want := []string{"inl_401000", "fn_401000", pythonFrameName(0xc0de0000), "inl_401100", "fn_401100"}
	if len(names) != len(want) {
		t.Fatalf("frames = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("frames = %v, want %v", names, want)
		}
	}
}

// The kernel-side merge must not disturb the user-side splice.
func TestKernelFramesStayLeafSideOfPythonFrames(t *testing.T) {
	sym := &countingSymbolizer{}
	slots, _ := splitFrameSlots(
		[]uint64{0x401000, 0xc0de0000, 0x5a5a5a5a},
		[]uint8{frameTagNative, frameTagPython, frameTagPython},
	)
	frames := symbolizePIDWithKernel(sym, fakeKernelSymbolizer{}, 4242, slots, []uint64{0xffff0000})
	if len(frames) != 3 {
		t.Fatalf("len(frames) = %d, want 3 (kernel, native, python)", len(frames))
	}
	if !frames[0].IsKernel {
		t.Error("kernel frames must stay leaf-side")
	}
	if frames[2].Name != pythonFrameName(0xc0de0000) {
		t.Errorf("the Python frame moved: %v", frames[2].Name)
	}
}

type fakeKernelSymbolizer struct{}

func (fakeKernelSymbolizer) Close() error { return nil }

func (fakeKernelSymbolizer) SymbolizeKernel(ips []uint64) ([]symbolize.Frame, error) {
	out := make([]symbolize.Frame, len(ips))
	for i, ip := range ips {
		out[i] = symbolize.Frame{Address: ip, Name: fmt.Sprintf("ksym_%x", ip)}
	}
	return out, nil
}

// A perf.data user-IP callchain is a list of instruction pointers by
// definition; every consumer resolves them against the process's mappings.
// A code object in that list is an address that is not code, in a format with
// no way to say so.
func TestPerfDataExportCarriesOnlyNativeIPs(t *testing.T) {
	slots, _ := splitFrameSlots(
		[]uint64{0x401000, 0xc0de0000, 0x5a5a5a5a, 0x401100},
		[]uint8{frameTagNative, frameTagPython, frameTagPython, frameTagNative},
	)
	ips := nativeIPs(slots)
	if len(ips) != 2 || ips[0] != 0x401000 || ips[1] != 0x401100 {
		t.Errorf("nativeIPs = %#x, want [0x401000 0x401100]", ips)
	}
}

// Two walks whose words agree but whose tags do not are different call paths
// and must not be deduped onto one another.
func TestHashStackSeparatesStacksThatDifferOnlyInTags(t *testing.T) {
	pcs := []uint64{0x401000, 0x401100}
	a := hashStack(pcs, []uint8{frameTagNative, frameTagNative})
	b := hashStack(pcs, []uint8{frameTagPython, frameTagPython})
	if a == b {
		t.Error("hashStack ignores the tags; two different call paths would share one sample key")
	}
}

// hashStack must be a pure function of its input: two walks that produced
// equal words and equal tags have to land on the same sample key, or the same
// stack is symbolized and reported several times over.
//
// The two operands are DISTINCT expressions over equal-but-separate data, not
// the same call twice. `f(x) != f(x)` has identical operands, so the compiler
// is free to fold it and the assertion can never fail -- this branch's
// recurring defect in yet another costume, and what staticcheck SA4000
// correctly refuses. Copying the inputs makes the comparison real: it fails
// for a hash that iterates a map, mixes in a per-call seed, or depends on the
// backing array rather than the values.
func TestHashStackIsAPureFunctionOfItsInput(t *testing.T) {
	pcs := []uint64{0x401000, 0xc0de0000, 0x5a5a5a5a}
	tags := []uint8{frameTagNative, frameTagPython, frameTagPython}

	samePCs := append([]uint64(nil), pcs...)
	sameTags := append([]uint8(nil), tags...)

	if hashStack(pcs, tags) != hashStack(samePCs, sameTags) {
		t.Error("equal chains hashed differently: hashStack is not a pure function of its input")
	}
	if hashStack(pcs, nil) != hashStack(samePCs, nil) {
		t.Error("equal chains with no tags hashed differently")
	}
}
