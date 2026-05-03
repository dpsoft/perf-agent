# Security Policy

## Reporting a vulnerability

**Please do not open a public issue.** perf-agent runs with elevated kernel capabilities (`CAP_SYS_ADMIN`, `CAP_BPF`, `CAP_PERFMON`, `CAP_SYS_PTRACE`, `CAP_CHECKPOINT_RESTORE`) and loads eBPF programs into the kernel; bugs in this surface can have privilege-escalation or denial-of-service implications, and we'd rather fix them before they're public.

**Preferred channel:** open a [GitHub Security Advisory](https://github.com/dpsoft/perf-agent/security/advisories/new) on this repository. GitHub keeps the report private until we publish it together.

**Alternative:** email `diegolparra@gmail.com` with `[perf-agent security]` in the subject line.

Please include:

- Affected version (commit SHA or release tag).
- Kernel version and distribution (`uname -r` plus `/etc/os-release` contents).
- A reproduction case — minimal command line, BPF program output, or test program.
- Your assessment of impact (DoS, info-leak, privilege escalation, integrity).
- Whether you've shared the report with anyone else.

We aim to acknowledge a report within 5 business days and to ship a fix or mitigation within 30 days for high-severity issues.

## Scope

In scope:

- Bugs in BPF programs that cause kernel crashes, info leaks, or unintended kernel-state changes.
- Userspace bugs that allow a non-root caller to escalate via the agent (e.g. crafted `/proc` content, malicious target processes).
- Bugs in capability handling — the agent dropping caps incorrectly, or running with broader caps than required.
- Symbolizer / ELF parser bugs that crash on malformed input from `/proc/<pid>/maps` targets.
- Issues in the optional Python perf-trampoline injector (`--inject-python`) — ptrace into untrusted targets, race conditions on attach.

Out of scope:

- Bugs that require an attacker to already have root or `CAP_SYS_ADMIN` on the host (that's the agent's required threat model — root can already do worse than this tool).
- Performance issues, including CPU/memory overhead that isn't denial-of-service shaped.
- Issues in dependencies that are reported and tracked upstream — link the upstream advisory and we'll coordinate the bump.
- General profiling correctness questions (file an issue instead).

## Threat model

perf-agent assumes:

- The user invoking the agent is trusted (root or capability-equivalent).
- The host kernel is trusted.
- Target processes (the things being profiled) are **not** trusted — the agent may run against malicious or compromised processes and must not be exploitable via crafted process state (`/proc` content, manipulated maps, race conditions on attach/detach).
- pprof output files are written with the user's umask and permissions; downstream consumers are responsible for their own access control.

The agent does not establish network connections, send telemetry, or talk to any external service. Everything stays local.

## Supported versions

This is a small, fast-moving project. Security fixes go to `main`. There are no maintained release branches today; if you're running a tagged release and need a backport, mention that in your report and we'll discuss.
