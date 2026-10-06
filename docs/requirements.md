# Requirements

- Linux kernel 5.8+ (BTF + CO-RE).
- Root, OR `setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep ./perf-agent`.

<details>
<summary>What each capability is for, and when <code>cap_sys_admin</code> can be dropped</summary>

| Capability | Why it is needed |
|---|---|
| `cap_bpf` | Load eBPF programs and create maps |
| `cap_perfmon` | `perf_event_open`, stack traces, tracing attachment |
| `cap_sys_ptrace` | Read `/proc/<pid>/maps` and `/proc/<pid>/mem` of the target |
| `cap_checkpoint_restore` | Follow `/proc/<pid>/map_files/` symlinks during symbolization |
| `cap_sys_admin` | Only on kernels older than 5.8/5.9 — see below |

`cap_checkpoint_restore` (or `cap_sys_admin`) is **required, not optional**.
blazesym reaches the file behind every mapping through
`/proc/<pid>/map_files/`, and the kernel refuses to follow those magic
symlinks without one of the two — so without it every user-space frame in a
profile is a bare hex address. perf-agent checks this at startup and refuses
to run rather than write a profile that looks like a result and is not.

`cap_sys_admin` is kept for backward compatibility. Two capabilities were added
to the kernel to carve out the roles perf-agent used it for — but they did not
arrive in the same release:

- **`CAP_PERFMON` (kernel 5.8)** covers `perf_event_open`, including `pid=-1`
  for system-wide profiling.
- **`CAP_CHECKPOINT_RESTORE` (kernel 5.9)** covers `/proc/<pid>/map_files`.

perf-agent's documented floor is kernel 5.8, and on exactly 5.8
`cap_checkpoint_restore` does not exist — so dropping `cap_sys_admin` there would
break symbolization. That single kernel minor version is why the full set is
still the default.

On **kernel 5.9 or newer** the minimal set is:

```bash
sudo setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep ./perf-agent
```

If you run 6.x — as most deployments now do — this is the set to use. It matters
most for per-pod and sidecar deployments, where `cap_sys_admin` is the near-root
capability that gets a workload rejected by admission policy.

</details>

---
