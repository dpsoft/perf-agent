package hip

import (
	"fmt"

	"github.com/cilium/ebpf"
)

type Objects struct {
	objs hiplaunchObjects
}

func Load(pid uint32) (*Objects, error) {
	spec, err := loadHiplaunch()
	if err != nil {
		return nil, fmt.Errorf("load hip launch spec: %w", err)
	}
	if err := spec.Variables["target_pid"].Set(pid); err != nil {
		return nil, fmt.Errorf("set target_pid: %w", err)
	}

	out := &Objects{}
	if err := spec.LoadAndAssign(&out.objs, nil); err != nil {
		return nil, fmt.Errorf("load and assign hip launch objects: %w", err)
	}
	return out, nil
}

func (o *Objects) LaunchProgram() any {
	return o.objs.HandleHipLaunch
}

func (o *Objects) EventsHandle() any {
	return o.objs.Events
}

func (o *Objects) StacksHandle() stackBytesLookup {
	return o.objs.Stackmap
}

func (o *Objects) EventsMap() *ebpf.Map {
	return o.objs.Events
}

func (o *Objects) StackMap() *ebpf.Map {
	return o.objs.Stackmap
}

func (o *Objects) Close() error {
	return o.objs.Close()
}
