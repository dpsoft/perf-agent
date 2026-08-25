# Removing the Python perf-trampoline injector

Branch: `chore/drop-python-trampoline` (one commit, not pushed).
Supersedes: the shipped `--inject-python` path. Replacement: issue
[#83](https://github.com/dpsoft/perf-agent/issues/83), **not yet built**.

## Why

Straight from #83, four objections to the injector:

1. **It mutates the process it measures.** It ptraces into a live interpreter and
   remote-calls `sys.activate_stack_trampoline('perf')`; the trampoline overhead
   then stays for the life of the process. The project's own standard elsewhere —
   Tier A must disclose that it serializes kernels, a heuristic join must be
   labelled a guess — is not met by a path that mutates a target and discloses
   nothing.
2. **CPython 3.12+ only.** `sys.activate_stack_trampoline` does not exist before
   3.12. PyTorch users are heavily on 3.10/3.11, so it mostly did not apply to
   the workload the project most wants to profile.
3. **It needs `CAP_SYS_PTRACE` for ptrace attach**, partially undoing #41's work
   toward running on `cap_bpf,cap_perfmon,cap_checkpoint_restore`.
4. **It cannot profile an already-running process** without injecting into it —
   which is the definition a continuous profiler has to meet.

## What was removed

**Packages (deleted outright, 1,960 lines of Go):**

- `inject/python/` — `detector.go`, `manager.go`, `payload.go`, `python.go`,
  `errors.go` + tests.
- `inject/ptraceop/` — `ptraceop.go`, `regs_amd64.go`, `regs_arm64.go` + tests.
  **Checked for other consumers: none.** Its only importer was
  `perfagent/agent.go`'s `ptraceopBridge`, which existed solely to adapt it to
  `inject/python`.
- `inject/elfsym/` — including `soname.go`, which the task flagged for a
  second look. **Checked: its only importer in the whole tree was
  `inject/python/detector.go:13`.** No other consumer, so it went with the rest.
  (`grep -rn "perf-agent/inject" --include='*.go'` now returns nothing.)

The `inject/` directory no longer exists.

**Wiring:**

- `main.go` — `flagInjectPython` declaration and the `WithInjectPython`
  plumbing in `buildOptions`.
- `perfagent/options.go` — `Config.InjectPython` field and `WithInjectPython`.
- `perfagent/agent.go` — the two `inject/*` imports, the `pyInjector` field,
  the `New()` construction block, both `validate()` rules, `hasCapSysPtrace`,
  the activate-before-attach and deactivate-after-finalize calls,
  `PythonInjectStats`, `scanPythonTargets`, `ptraceopBridge` (+ its two
  methods), `mapPtraceopErrToPython`, and `dwarfHooksForAgent`. The now-unused
  `strconv` import went too.
- `bench/cmd/scenario/main.go` — a stale `NOTE:` block explaining why
  `--inject-python` was *not* plumbed into the bench. It referenced
  `python.Manager`, a type that no longer exists.

**Tests:** `test/integration_inject_python_test.go`, plus the unit tests of
every deleted package (they went with their packages).

**Docs:** `docs/python-profiling.md` (deleted — the whole file was about
injection); the `--inject-python` row in the README flag table; the injector
bullet in `SECURITY.md`'s in-scope list.

## What was kept, and why

- **`pprof/pprof.go`'s `decodePython` / `decodePerfMapFrame`.** This parses
  `py::<qualname>:<file.py>` lines out of `/tmp/perf-<pid>.map`. Those lines are
  written by *CPython itself* when the interpreter is started with `-X perf` /
  `PYTHONPERFSUPPORT=1` — the injector was only one of two ways to turn that on,
  and the other (the user launching the interpreter that way) is unaffected. This
  is a symbolization decoder, not part of the injection path. Removing it would
  have deleted working functionality.
- **`test/PYTHON_PROFILING.md`, `test/PYTHON_PERF_KNOWN_ISSUES.md`,
  `test/check_python_perf.sh`, `test/test_python_profile.sh`, the Python
  workloads, and the `-X perf` sections of `BUILDING.md` / `TESTING.md`.**
  Verified by grep: none of them mention `--inject-python`, `trampoline`, or
  injection at all. They document the `-X perf` route, which still works.
- **`unwind/dwarfagent.Hooks.OnNewExec` and `ehmaps.PIDTracker.SetOnNewExec`.**
  The injector was `OnNewExec`'s only production consumer, but the hook is an
  exported, tested, general-purpose observation surface in a package this task
  is not permitted to touch — and #83's walker plausibly wants exactly it for
  late-arriving interpreters. `perfagent` now passes `nil` for the `hooks`
  argument at both `NewProfilerWithMode` call sites instead of calling a
  function that could only ever return `nil`.
- **The 1.1.0 `CHANGELOG.md` entry announcing the injector.** That is history and
  was true when written. The removal is recorded under `[Unreleased] → Removed`
  instead, per Keep a Changelog.

## Capability requirements: unchanged

**`CAP_SYS_PTRACE` stays, and stays documented.** Injection was *not* its only
consumer. Checked before touching any doc:

- `perfagent/agent.go`'s `Start()` raises it to Effective so blazesym can read
  `/proc/<pid>/maps` and `/proc/<pid>/mem` of *other users'* processes — the
  ordinary system-wide and cross-uid profiling case. Same for
  `profile/dwarf_export.go` and `profile/offcpu_dwarf_export.go`.
- `gpuprobe/enroll.go`'s `enrollRequiredUID()` uses the presence of
  `CAP_SYS_PTRACE` to decide whether the enrollment rendezvous may serve
  producers belonging to other uids.
- `symbolize/local.go` prints it in its setcap hint.

So there is **no capability improvement to claim here**, and none is claimed. The
README capability table, the `setcap` lines, and the `Makefile` cap hints are all
untouched. The only `CAP_SYS_PTRACE` text that went away is the injector-specific
mention in the `--inject-python` flag help and in `docs/python-profiling.md`'s
`ptrace_scope` troubleshooting — both of which described ptrace *attach*, a use
that no longer exists. Reading another process's `/proc` entries still does.

The `hasCapSysPtrace()` helper in `perfagent/agent.go` was deleted because
`validate()` was its only caller. The functionally identical check in
`gpuprobe/enroll.go` is untouched; its comment cross-references
"perfagent's hasCapSysPtrace" and is now a stale pointer — noted below.

## What users lose, and where the docs say so

**perf-agent now produces no Python-level frames of its own, on any Python
version, with no replacement available.** A Python process still profiles
normally, but the stacks show C interpreter frames (`_PyEval_EvalFrameDefault`,
…) rather than Python qualnames. The only remaining route to Python names is one
perf-agent does not control: whoever launches the interpreter starting it with
`python -X perf` (3.12+), after which CPython writes `/tmp/perf-<pid>.map` itself
and perf-agent reads and decodes it exactly as before.

The replacement — walking `PyRuntimeState → PyThreadState → _PyInterpreterFrame`
from BPF, needing no injection and covering CPython 3.6+ — is #83, which is
sequenced **after** the GPU PC-sampling phase and has not been built.

Stated plainly in two user-facing places, both naming #83 and linking it:

- **`README.md`**, in a blockquote directly under "🐍 Cross-language flame
  graphs" — the section that previously advertised Python support. The section
  body no longer lists Python among the symbolized runtimes, and the "On-demand
  production profiling" blurb lost its `--inject-python` sales pitch. The
  "For Python workloads, see docs/python-profiling.md" pointer near the examples
  now points at that note instead of a deleted file.
- **`CHANGELOG.md`**, under `[Unreleased] → Removed`, with the four reasons, the
  explicit "this removes a shipped capability with no replacement yet", and the
  `-X perf` carve-out.

The commit message carries the same statement.

## Straggler grep

`grep -rn <term> --exclude-dir=.git .` over the whole tree, after the change:

| Term | Hits | Verdict |
|---|---|---|
| `ptraceop` | 0 | clean |
| `elfsym` | 0 | clean |
| `activate_stack_trampoline` | 2, both `CHANGELOG.md` | intentional: the removal entry and the historical 1.1.0 entry |
| `inject-python` | 4 | 2 in `CHANGELOG.md` and 1 in `README.md` are the intentional removal notices; **1 stale — see below** |
| `trampoline` | many | all unrelated: `bpf/vmlinux*.h` kernel BTF (`bpf_trampoline`, `ftrace_trampolines`, …), `test/integration_test.go` comments about JIT trampoline memory, plus the CHANGELOG/README notices |
| `PYTHONPERFSUPPORT` | 4 | all about `-X perf`, which is retained: `pprof/pprof.go` (×2, the decoder's format docs), `test/PYTHON_PERF_KNOWN_ISSUES.md`, `test/WARMUP_IMPLEMENTATION.md` |
| `InjectPython` / `PythonInjectStats` / `pyInjector` | 0 | clean |
| `python-profiling` (doc link) | 0 | no dangling link to the deleted file |

**One known stale reference, left deliberately:**

`unwind/ehmaps/tracker.go:58` — the doc comment on `SetOnNewExec` ends
"…zero producer overhead when `--inject-python` is off." The flag no longer
exists, so the sentence is now meaningless. It lives in `unwind/`, which this
task explicitly forbade touching, and the fix is a one-line comment reword with
no functional content. **Follow-up needed**, e.g. "…when no hook is registered
(the default)". Verified that none of the other active worktrees
(`pc-snapshot-join`, `tier-b-sampling`, `flamegraph-views`,
`pending-module-index`, `frame-modules`) modify `unwind/`, so the change will
not conflict whenever someone makes it.

Two lower-priority stale cross-references in the same category (also outside this
task's remit, both comment-only):
`gpuprobe/enroll.go:610` and `gpuprobe/gate_test.go:28` say they mirror
"perfagent/agent.go's `hasCapSysPtrace`", a function this commit deleted. Their
*logic* is correct and self-contained; only the pointer is stale.

## Also superseded: branch `feat/python-perf-injector`

The unmerged branch `feat/python-perf-injector` (~6,181 lines, worktree at
`.worktrees/python-perf-injector`) builds further on the injector. It was **not
touched**. It is now superseded by this removal and by #83, and **should be
closed** along with any PR opened from it.

## Verification

All run from the worktree with the project's CGO flags:

```
go build ./...                                        # clean
go vet ./...                                          # clean
go test ./... -count=1                                # all pass, 0 failures
cd test && go vet -tags integration ./...             # clean
cd test && go test -c -tags integration -o /dev/null ./...   # compiles
~/go/bin/golangci-lint run --timeout=5m               # 0 issues
```

No privilege was required or used (`CapEff: 0`).
