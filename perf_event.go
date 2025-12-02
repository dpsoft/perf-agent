package main

import (
	"fmt"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/elastic/go-perf"
	"golang.org/x/sys/unix"
)

type perfEvent struct {
	fd    int
	ioctl bool
	link  *link.RawLink
	p     *perf.Event
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
	//pe.p.Close()
	_ = syscall.Close(pe.fd)
	if pe.link != nil {
		_ = pe.link.Close()
	}
	return nil
}

func (pe *perfEvent) attachPerfEvent(prog *ebpf.Program) error {
	return pe.p.SetBPF(uint32(prog.FD()))
	//err := pe.attachPerfEventLink
	//if err == nil {
	//	return nil
	//}
	//return pe.attachPerfEventIoctl(prog)
}

func (pe *perfEvent) attachPerfEventIoctl(prog *ebpf.Program) error {
	var err error
	err = unix.IoctlSetInt(pe.fd, unix.PERF_EVENT_IOC_SET_BPF, prog.FD())
	if err != nil {
		return fmt.Errorf("setting perf event bpf program: %w", err)
	}
	if err = unix.IoctlSetInt(pe.fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
		return fmt.Errorf("enable perf event: %w", err)
	}
	pe.ioctl = true
	return nil
}

func (pe *perfEvent) attachPerfEventLink(prog *ebpf.Program) error {
	var err error
	opts := link.RawLinkOptions{
		Target:  pe.fd,
		Program: prog,
		Attach:  ebpf.AttachPerfEvent,
	}

	pe.link, err = link.AttachRawLink(opts)
	if err != nil {
		return fmt.Errorf("attach raw link: %w", err)
	}

	return nil
}
