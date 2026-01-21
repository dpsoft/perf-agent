package main

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/elastic/go-perf"
)

type perfEvent struct {
	fd   int
	link *link.RawLink
	p    *perf.Event
}

func newPerfEvent(cpu int, sampleRate int) (*perfEvent, error) {
	perfAttribute := new(perf.Attr)
	perfAttribute.SetSampleFreq(uint64(sampleRate))
	perfAttribute.Type = unix.PERF_TYPE_SOFTWARE
	perfAttribute.Config = unix.PERF_COUNT_SW_CPU_CLOCK

	p, err := perf.Open(perfAttribute, -1, cpu, nil)

	if err != nil {
		return nil, fmt.Errorf("open perf event: %w", err)
	}

	fd, err := p.FD()
	if err != nil {
		return nil, fmt.Errorf("get perf event fd: %w", err)
	}

	return &perfEvent{fd: fd, p: p}, nil
}

func (pe *perfEvent) Close() error {
	_ = syscall.Close(pe.fd)
	if pe.link != nil {
		_ = pe.link.Close()
	}
	return nil
}

func (pe *perfEvent) attachPerfEvent(prog *ebpf.Program) error {
	return pe.p.SetBPF(uint32(prog.FD()))
}
