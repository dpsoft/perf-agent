package linuxdrm

import (
	"fmt"

	"github.com/cilium/ebpf"
)

type Objects struct {
	objs linuxdrmObjects
}

func Load(pid uint32) (*Objects, error) {
	spec, err := loadLinuxdrm()
	if err != nil {
		return nil, fmt.Errorf("load linuxdrm spec: %w", err)
	}
	if err := spec.Variables["target_pid"].Set(pid); err != nil {
		return nil, fmt.Errorf("set target_pid: %w", err)
	}

	out := &Objects{}
	if err := spec.LoadAndAssign(&out.objs, nil); err != nil {
		return nil, fmt.Errorf("load and assign linuxdrm objects: %w", err)
	}
	return out, nil
}

func (o *Objects) EnterProgram() *ebpf.Program {
	return o.objs.HandleEnterIoctl
}

func (o *Objects) ExitProgram() *ebpf.Program {
	return o.objs.HandleExitIoctl
}

func (o *Objects) EventsMap() *ebpf.Map {
	return o.objs.Events
}

func (o *Objects) Close() error {
	return o.objs.Close()
}
