package pyunwind

// The CPython frame walker is its OWN BPF object, generated into this package
// rather than into profile/ or gpuprobe/, because this package is what owns
// it: py_procs, py_walk_counters and py_frame_scratch are its maps, and the
// per-version offsets they carry are computed a few files away.
//
// It shares nothing with the drivers' objects but bpf/unwind_record.h. At load
// time unwind/interp binds its walker_scratch, walk_states and interp_progs to
// the driver's own (cilium/ebpf MapReplacements) and installs the program
// whose type matches the driver into the driver's interp_progs table.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=pyunwind pywalk ../bpf/interp/python/python_walk.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=pyunwind pywalk ../bpf/interp/python/python_walk.bpf.c -- -I../bpf
