// Package modules is the ONE place in this tree that names an interpreter.
//
// Importing it registers every language module the binary should carry. Every
// other package -- profile, gpuprobe, unwind/dwarfagent, pprof -- depends on
// unwind/interp, which names none of them, so adding a language touches this
// file and the language's own package and nothing else.
//
// It is a separate package from unwind/interp on purpose: interp must stay
// importable by a module (a module implements interp.Module), and a registry
// that imported its own modules would be a cycle.
package modules

import (
	"github.com/dpsoft/perf-agent/pyunwind"
	"github.com/dpsoft/perf-agent/unwind/interp"
)

func init() {
	// CPython. See bpf/interp/python/ for the BPF side and pyunwind for the
	// per-version offsets it needs.
	interp.Register(pyunwind.Module)
}
