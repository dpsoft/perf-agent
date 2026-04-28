package hip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	blazesym "github.com/libbpf/blazesym/go"

	pp "github.com/dpsoft/perf-agent/pprof"
)

type fakeProcessSymbolizer struct {
	t        *testing.T
	syms     []blazesym.Sym
	err      error
	lastPID  uint32
	lastAddr []uint64
}

func (s *fakeProcessSymbolizer) SymbolizeProcessAbsAddrs(addrs []uint64, pid uint32, _ ...blazesym.ProcessSourceOption) ([]blazesym.Sym, error) {
	s.lastPID = pid
	s.lastAddr = append([]uint64(nil), addrs...)
	return s.syms, s.err
}

func (s *fakeProcessSymbolizer) Close() {}

type fakeStackLookup struct {
	data map[uint32][]byte
	err  error
}

func (l fakeStackLookup) LookupBytes(key interface{}) ([]byte, error) {
	if l.err != nil {
		return nil, l.err
	}
	id, ok := key.(uint32)
	if !ok {
		return nil, fmt.Errorf("unexpected key type %T", key)
	}
	return l.data[id], nil
}

func TestResolveKernelNameUsesSymbolizedName(t *testing.T) {
	sym := &fakeProcessSymbolizer{
		syms: []blazesym.Sym{{Name: "hip_kernel_stub"}},
	}
	got := resolveKernelName(sym, 123, 0x1234)
	if got != "hip_kernel_stub" {
		t.Fatalf("kernel=%q", got)
	}
	if sym.lastPID != 123 {
		t.Fatalf("pid=%d", sym.lastPID)
	}
	if len(sym.lastAddr) != 1 || sym.lastAddr[0] != 0x1234 {
		t.Fatalf("addrs=%#v", sym.lastAddr)
	}
}

func TestResolveKernelNameFallsBackToAddress(t *testing.T) {
	sym := &fakeProcessSymbolizer{err: errors.New("boom")}
	got := resolveKernelName(sym, 123, 0x1234)
	if got != "hip_kernel@0x1234" {
		t.Fatalf("kernel=%q", got)
	}
}

func TestResolveStackFramesSymbolizesBatch(t *testing.T) {
	var stack bytes.Buffer
	for _, ip := range []uint64{0x1000, 0x2000, 0} {
		if err := binary.Write(&stack, binary.LittleEndian, ip); err != nil {
			t.Fatalf("write ip: %v", err)
		}
	}

	sym := &fakeProcessSymbolizer{
		syms: []blazesym.Sym{
			{Name: "leafA"},
			{Name: "leafB"},
		},
	}
	frames := resolveStackFrames(sym, fakeStackLookup{
		data: map[uint32][]byte{7: stack.Bytes()},
	}, 123, 7)
	if len(frames) != 2 {
		t.Fatalf("frames=%d", len(frames))
	}
	if frames[0].Name != "leafA" || frames[1].Name != "leafB" {
		t.Fatalf("frames=%+v", frames)
	}
}

func TestResolveStackFramesFallsBackToUnknownFrames(t *testing.T) {
	var stack bytes.Buffer
	for _, ip := range []uint64{0x1000, 0x2000, 0} {
		if err := binary.Write(&stack, binary.LittleEndian, ip); err != nil {
			t.Fatalf("write ip: %v", err)
		}
	}

	frames := resolveStackFrames(&fakeProcessSymbolizer{err: errors.New("boom")}, fakeStackLookup{
		data: map[uint32][]byte{7: stack.Bytes()},
	}, 123, 7)
	if want := []pp.Frame{
		{Name: "[unknown]", Address: 0x1000},
		{Name: "[unknown]", Address: 0x2000},
	}; len(frames) != len(want) || frames[0] != want[0] || frames[1] != want[1] {
		t.Fatalf("frames=%+v", frames)
	}
}
