# Python profiling with perf-agent

perf-agent supports two paths for Python frame symbolization:

1. **DWARF unwinding through the interpreter** (default for any Python).
   Profiles work; frames render with C-level names
   (`_PyEval_EvalFrameDefault`, etc.) — no Python-level qualnames.

2. **Perf trampoline** (`--inject-python`, Python 3.12+). Activates
   CPython's built-in perf integration so perf-agent can resolve every
   JIT'd Python function to its qualname.

## Quickstart

```bash
# Profile a running Python web server for 30 seconds
sudo perf-agent --profile --pid $(pgrep -f gunicorn) \
                --duration 30s --inject-python \
                --profile-output gunicorn.pb.gz

# Visualize
go tool pprof -http :8080 gunicorn.pb.gz
```

## How it works

When `--inject-python` is set, perf-agent:

1. Walks `/proc` (or just the target PID) and identifies CPython 3.12+
   processes via libpython SONAME and ELF symbol presence.
2. For each candidate, attaches via `ptrace`, calls
   `PyGILState_Ensure` → `PyRun_SimpleString("import sys; sys.activate_stack_trampoline('perf')")` → `PyGILState_Release`,
   and detaches.
3. The trampoline emits perf-map entries to `/tmp/perf-<PID>.map`.
4. perf-agent samples and reads the perf-map via blazesym to attach
   Python names to frames.
5. On profile end, the same ptrace dance runs
   `sys.deactivate_stack_trampoline()` so the trampoline overhead does
   not persist.

## When injection is skipped

| Reason | Cause |
|---|---|
| `not_python` | Target is not a CPython process (e.g., Go binary). |
| `python_too_old` | libpython version < 3.12 (`activate_stack_trampoline` is 3.12+). |
| `no_perf_trampoline` | CPython compiled without `--enable-perf-trampoline` (e.g., some Alpine builds). |
| `no_libpython_symbols` | Statically-linked CPython without exported PyGILState/PyRun symbols. |

In `--pid N` mode (strict), any of these failures aborts the run.
In `-a` mode (lenient), failures are logged and the profile continues
for all targets where injection succeeded.

## Continuous profiling

Activation is idempotent: each run activates → profiles → deactivates.
The `/tmp/perf-<PID>.map` file persists between runs (we don't delete
it), and the next activation appends new entries. This is the supported
pattern for continuous profiling.

If the target was launched with `python -X perf`, the deactivate-at-end
will turn the trampoline off; users who want to keep `-X perf` always-on
should not pass `--inject-python`.

## Container and namespace caveats

v1 uses host-side PIDs. If perf-agent runs outside a container and
targets a Python process inside one:

- The host PID works for ptrace and detection
  (`/proc/<pid>/maps` + on-disk libpython path).
- The perf-map file `/tmp/perf-<host_pid>.map` is created on the host —
  the container itself does not see it. This matches `python -X perf`
  behavior under host-mounted `/tmp`.
- For exotic mount namespace setups, detection may fail with "library
  not found on disk" — log + skip in lenient mode.

A future PR can add namespace-aware path resolution; the seam is small
(one function: "given pid, give me the on-disk libpython path"). File
an issue if you hit this.

## Performance impact

The CPython 3.12 perf trampoline adds 1–5% per-call overhead on hot
Python workloads, depending on call shape. For typical web servers and
pipelines, overhead is in the noise. perf-agent's deactivation pass at
end of profile removes this overhead immediately.

## Disabling injection

Don't pass `--inject-python`. Profiles still work — Python frames just
render with C interpreter names instead of qualnames.

## Troubleshooting

**`ptrace_eperm` errors:** the target's
`/proc/sys/kernel/yama/ptrace_scope` is restricting ptrace. Set `0`
(or `1` for same-uid attach), or grant `CAP_SYS_PTRACE` to perf-agent
(already in the standard cap set).

**`ESRCH` during deactivate:** the target exited during the profile.
Harmless; logged with `pid=N reason=process_gone`.

**`/tmp/perf-PID.map` missing after activation:** the target may not
have called any new Python code during the profile, so the trampoline
had nothing to emit. Lengthen `--duration`.

**Statically-linked Python skipped (`no_libpython_symbols`):** the
binary's symbol table is stripped. Distributions that ship
`python-build-standalone` sometimes do this. No workaround in v1.
