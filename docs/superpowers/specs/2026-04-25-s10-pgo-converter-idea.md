# S10 — pprof → LLVM Sample-Profile Converter (Idea Note)

> **Status:** idea capture only. NOT a design spec, NOT a plan. This file
> exists so the thought isn't lost between sessions. Brainstorm into a
> proper spec before implementation.

## Why

S9 gave perf-agent's pprof output real per-binary mappings + build-ids + address-keyed locations. Downstream consumers can now attribute samples to ELF file offsets — the prerequisite for sample-based PGO.

The user's stated goal: feed Rust PGO from a perf-agent profile.

```
perf-agent --profile --pid <rust-bin>   →  profile.pb.gz
                                            │
                                            ▼   <-- S10 lives here
                                   pprof2llvm / equivalent
                                            │
                                            ▼
                                  profile.llvm.txt or .profdata
                                            │
                                            ▼
                              llvm-profdata merge --sample
                                            │
                                            ▼
                                     profile.profdata
                                            │
                                            ▼
                          cargo rustc --release -Cprofile-use=profile.profdata
```

## Two paths worth comparing before designing

### Path A — direct pprof → LLVM sample profile

Write a Go converter that reads a perf-agent pprof and emits the LLVM sample-profile **text format**:

```
function_name:total_samples:head_samples
 line_offset: sample_count
 line_offset: sample_count  callee_function:samples
 ...
```

Then `llvm-profdata merge --sample profile.txt -o profile.profdata`.

**Hard parts:**
- LLVM keys by *function-relative line offset*, not absolute file line. We need `func_start_line` per function. pprof doesn't carry it; blazesym doesn't directly expose it. Plumbing this means walking DWARF DIEs at conversion time, or extending S9's symbolizer to retain it.
- LLVM keys by *linker symbol name*. blazesym returns demangled names by default. For Rust we'd need raw mangled names (`_RNvXNtCs...`) or a guaranteed inverse demangle. Either re-symbolize at conversion time, or extend the pprof emission to store raw names alongside demangled.
- *Body samples* vs *callsite samples* — LLVM distinguishes "leaf sample at line X" from "non-leaf line X is a callsite to function Y". pprof's sample chain encodes this implicitly (leaf vs non-leaf in the stack); the converter has to make it explicit.
- *Inline expansion* — pprof already expands inlined chains. LLVM sample profile encodes inlines with nested syntax. Faithful round-trip needs care.
- *Discriminators* — S9 deferred; column-level / discriminator info isn't in pprof today. Without it, two statements on the same line collapse. Tolerable for a v1.

**Pros:** self-contained, no perf.data writer needed, feeds Rust PGO directly. Output is human-readable text — easy to debug.
**Cons:** reimplements logic that already exists in autofdo / llvm-profgen.

### Path B — perf-agent emits perf.data, reuse existing tooling

Teach perf-agent to write Linux `perf.data` format (PERF_RECORD_SAMPLE + PERF_RECORD_MMAP2 + PERF_RECORD_COMM + build-id table), then point existing tools at it:

```
perf-agent --profile --pid <bin> --perf-data-output profile.perf.data
create_llvm_prof --binary=<bin> --profile=profile.perf.data --out=profile.llvm.txt
llvm-profdata merge --sample profile.llvm.txt -o profile.profdata
```

`create_llvm_prof` (Google autofdo) and `llvm-profgen` (newer, LBR-aware) already consume perf.data and emit LLVM sample profiles. They handle line offsets, symbol mangling, body/callsite distinction, inline expansion correctly because that's what they're built for.

**Hard parts:**
- Implementing a perf.data writer is non-trivial but well-specified (kernel headers + `tools/perf` source as ground truth).
- Drag of an external dependency at the user's PGO pipeline (autofdo or LLVM tools).

**Pros:** inherits years of LLVM/autofdo work; gets LBR support for free if we ever feed branch records; perf.data is an ecosystem standard (works with `perf script`, FlameGraph, hotspot, magic-trace).
**Cons:** perf.data writer is meaningful new code; less direct than Path A.

### Path A vs B — which to pick

Probably **B** if we're serious about PGO. Path A duplicates well-trodden conversion logic and the failure modes (mangled names, line offsets, callsite detection) are exactly the things autofdo's authors have already solved. perf.data is also more useful as an *output* — flame graphs, hotspot inspection, LBR-aware tools — for users who don't care about PGO specifically.

**A** wins if we want zero external tools and are OK with a less-faithful first cut.

A reasonable hybrid: Path A as a 1-week exploration to validate the data we have is sufficient (if the converter can produce *something* `llvm-profdata` accepts, S9's address+mapping plumbing is vindicated end-to-end); Path B as the production answer.

## Open questions to resolve before designing

- **Which Rust toolchain version are we targeting for PGO?** rustc's PGO support has matured over time; the `.profdata` format expectations differ between LLVM 14 and LLVM 18.
- **AutoFDO vs IR-level PGO?** rustc supports both; sample-based (AutoFDO) is what perf-agent's data shape fits, but the user should confirm that matches their goal.
- **What's the workload-validation story?** PGO without a representative training run is worse than no PGO. We'll need a recommended capture protocol (duration, system load, --pid vs -a).
- **Symbol stability.** rustc -Cprofile-use compares against build-id + symbol names from the *new* compile; if the optimizer reorders or rename-mangles differently between training and use builds, samples are misattributed silently. Path B inherits autofdo's mitigation; Path A would need to invent one.

## Out of scope for S10 (defer to S11+)

- C++ PGO support. Path B handles it for free; Path A doesn't, but we don't need to.
- LBR (Last Branch Record) ingestion. perf-agent doesn't capture LBR today. Adding it touches BPF + userspace and is its own project.
- Continuous/always-on PGO sampling. The current capture model is one-shot per profile session.
- Differential PGO (compare two captures, emit "what got hotter"). Useful but separate.

## Recommended next step

Brainstorm session to pick Path A vs B (or hybrid), define the binary-info-retention protocol (mangled symbol names, function start lines, discriminators), and produce a real spec. Implementation plan only after that.
