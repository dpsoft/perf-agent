# Contributing to perf-agent

Thanks for your interest. perf-agent is an eBPF-based Linux profiler — a small project, mostly maintained by one person, but contributions are welcome.

## Quick links

- **Build & toolchain setup:** [BUILDING.md](./BUILDING.md)
- **Test layout:** see `test/` directory and `make test-unit` / `make test-integration`
- **Architecture & internals:** [README.md](./README.md) (Architecture section). Forward-looking specs (e.g. GPU profiling) live in `docs/superpowers/specs/`; shipped specs are not retained — the README and code are the source of truth once a feature lands.
- **Vulnerability reports:** [SECURITY.md](./SECURITY.md) — please do **not** open public issues
- **Conduct:** [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md)

---

## Before you open a PR

1. **Talk first if it's substantial.** For anything bigger than a bugfix, please open an issue describing the problem and the proposed approach. Multi-step features may benefit from a design spec under `docs/superpowers/specs/` before implementation; the existing GPU profiling spec is one example of the level of detail used.
2. **Run the full test suite.** Unit tests don't need root:
   ```bash
   make test-unit
   ```
   Integration tests require root or appropriate capabilities — see BUILDING.md for the exact setcap or sudo invocation.
3. **Lint:**
   ```bash
   golangci-lint run --timeout=5m
   ```
   CI runs this and it must pass before merge.
4. **Cross-arch:** the project supports `amd64` and `arm64`. For BPF-touching changes, regenerate bytecode with `make generate` and verify both architectures still build.

## Commit & PR conventions

- **One logical change per commit.** A refactor commit and a behaviour-change commit don't belong in the same commit.
- **Commit-message body explains *why*, not *what*.** The diff already shows the what.
- **No `Co-Authored-By:` lines** unless the work was genuinely co-authored by another human.
- **PR descriptions** state the problem first, the change second. Non-goals matter — call out what you're explicitly *not* doing in this PR. Look at recently merged PRs (#12, #13, #14) for shape.
- **Tests with the change.** Bug fixes should have a regression test that fails before the fix.

## Code style

- Modern Go (the project tracks the latest Go release). Use `slices`, `maps`, `strings.Lines`, `strings.SplitSeq`, `errors.Is`, `t.Setenv`, and other recent stdlib idioms instead of older equivalents.
- Default to writing no comments. Only add a comment when the *why* is non-obvious — a hidden constraint, a workaround for a specific bug, behaviour that would surprise a reader. Don't comment on what well-named identifiers already explain.
- Don't add features, refactors, or abstractions beyond what the change requires. A bug fix doesn't need surrounding cleanup.
- For BPF code in `bpf/`, follow the existing patterns — small programs, explicit map types, no shared global state across programs.

## Review process

- All changes go through pull request, even for the maintainer.
- CI must be green: lint, build (amd64 + arm64), unit tests (amd64 + arm64), integration tests (amd64 + arm64).
- Substantive PRs (>~200 LoC or touching BPF) typically get a review pass for: BPF verifier impact, capability requirements, and userspace memory safety around BPF maps.

## Filing issues

- **Bugs:** include kernel version (`uname -r`), distro, Go version, full command line that reproduced the issue, and the relevant log output. If a profile is involved, attach it (or describe how to reproduce one).
- **Feature requests:** describe the user-visible problem first. The feature is one possible answer; the problem is the input.
- **Security issues:** see [SECURITY.md](./SECURITY.md). Do not open a public issue.

## Scope

perf-agent is intentionally a tool, not a service. Things that fit:

- New profiling modes (e.g. heap, lock contention) that produce pprof output.
- Symbol resolution improvements.
- New runtime support for inlined frame extraction (e.g. Java, Ruby).
- Performance and overhead reductions in BPF / userspace.
- Better diagnostics in failure paths.

Things that don't:

- Cluster-wide watchers, scrape configs, or always-on daemon modes — that's the OTel / Pyroscope / Parca model. perf-agent is the engine; build that on top of it as a separate project.
- Cloud backends or telemetry. perf-agent runs entirely local.
- Output formats that aren't pprof or a small set of plain-text PMU summaries.
