# Task 6 — rebase onto `origin/main` (`a81727ec`)

PR #81 was written against `ccd0fe5a`. Tasks 5 and 7 merged after it, so the
branch went `CONFLICTING`. This is the record of the four conflicts, how each
was resolved, and what was actually run to confirm that neither side's
behaviour was dropped.

**One correction to the brief up front: Task 8a is not on `main`.** The brief
says Tasks 5, 7 and 8a merged ahead of this one, but `origin/main` carries only
Task 5 (`19d132db`) and Task 7 (`e3d46b25`); Task 8a is still the unmerged
branch `origin/feat/pc-pending-module-index`, and there is no
`task-8a-pending-index-report.md` in the tree. `GPUPCSample.FunctionIndex` does
not exist on `main` — Task 7's report names adding it as Task 8a's job. So
`gpuprobe/consumer.go` is a two-way merge (Tasks 6 and 7), not a three-way one,
and nothing here needed to accommodate a `FunctionIndex` field that is not yet
there.

---

## 1. `shim/Makefile` — a union of test targets

One conflict, on the `test:` prerequisite line. Task 5 added
`check-cubin-defer` and `core/cubinqueue_test.cc`; Task 6 added
`core/pcdrain_test.cc`. Resolved as the union of all three. The recipe body
below the prerequisite line merged cleanly and already contained both new
compile-and-run lines.

The whole delta against `main` is now one changed prerequisite line and one
added recipe line — no removals:

```
-test: check-cubin-defer … core/cubinqueue_test.cc core/drain_test.cc core/enroll_test.cc …
+test: check-cubin-defer … core/cubinqueue_test.cc core/drain_test.cc core/pcdrain_test.cc core/enroll_test.cc …
+	$(CXX) -std=c++17 -pthread -I core -o /tmp/pcdrain_test core/pcdrain_test.cc && /tmp/pcdrain_test
```

`make -C shim test` runs every target from both tasks, `check-cubin-defer`
included as a prerequisite.

---

## 2. `gpuprobe/consumer.go` — all arms survive, and one comment stopped being true

One conflict, in the `batch` struct. Task 7 added `Modules []gpuabi.ModuleLoad`
and `PCSamples []gpuabi.PCSample`; Task 6 had a comment block in the same place
explaining that the stall-map / window / config / drop kinds were decoded but
not yet normalized. Both fields kept; the comment kept but narrowed to `Drops`
alone, because Task 7 gave the other three `applyBatch` arms and the comment
would otherwise assert something false.

Everything else in this file merged textually, but **the textual merge left two
false statements behind, which is the more interesting half of this conflict.**
Task 7 wrote, in `Stats.Undecoded`'s doc and in the `default:` arm, that *"no
kind the ABI defines reaches here any more"*. Task 6 adds `kindDropped = 10`
with a cookie and a `decodeBatch` arm but no `applyBatch` arm — Task 6's report
defers drop normalization to Task 7, and Task 7 (written without `kindDropped`
in the tree) did not pick it up. So after the merge exactly one ABI-defined kind
does still land in `default:`.

Nothing was changed to make the comment true — that would have meant writing a
new `applyBatch` arm, which is neither task's work and is beyond a rebase.
Instead the three comments now say what is actually the case: `kindDropped` is
the one defined kind still counted as `Undecoded`, and its normalization is the
consumer task that follows. The three sites are `Stats.Undecoded`'s doc, the
`default:` arm, and the header comment on
`TestUndecodedKindsAreCountedNotDropped`.

Arms confirmed present after the merge: `decodeBatch` has `kindLaunch`,
`kindExec`, `kindModule`, `kindPC`, `kindLaunchSampled`, `kindKernelName`,
`kindStallMap`, `kindSamplingWindow`, `kindConfig`, `kindDropped`;
`applyBatch` has all of those except `kindDropped`; `cookieFor` names all ten.

### The `kindMax` / `cookieFor` completeness test did its job

`TestEveryProducerProbeHasACookieAndViceVersa` — the test Task 6 added to
assert that every probe a producer fires has a cookie and every cookie names a
fired probe — **failed on the first post-merge run**, and correctly:

```
Error:    map[…"gpu_module_load_v1":"the adapter captures cubins (Task 5)"…]
          should not contain "gpu_module_load_v1"
Messages: gpu_module_load_v1 is fired by a producer now; drop it from
          notYetFired so the list keeps meaning what it says
```

Task 6 listed `gpu_module_load_v1` as a cookie with no producer, attributed to
Task 5. Task 5 has landed and both producers now fire it. The fix is the one
the test's own message prescribes: the `notYetFired` exemption list shrinks from
two entries to one (`gpu_sampling_window_v1`, Task 10). That is the direction
the list was built to move in, and it moved because the assertion forced it —
not because anyone remembered.

---

## 3. `shim/stub/stub.cc` — both emitters, and Task 6's ordering preserved

Three conflicts, all pure unions, all resolved with both sides intact:

| where | Task 5 | Task 6 |
| --- | --- | --- |
| emitter block | `gpu_module_load_v1` | the four PC-sampling probes, `kStallNames`, `kStubCubinCRC` |
| `main` locals | `CubinQueue cubins` + `cubin_timeout_ms` | `PERFAGENT_STUB_PC_SAMPLES`, `ReplayLog` + its two replay hooks |
| after the launch loop | the final `cubins.drain(...)` pass | the whole synthesized Tier B block |

**Task 6's deliberate emission order is preserved.** Inside the Tier B block
the PC batches are still emitted and flushed *before* the stall map, which is
before the config record, which is before the four drop records — the ordering
Task 6 chose so a stall index arrives unresolvable and Task 7's
`pendingStallSamples` path is actually exercised rather than bypassed.

In the third hunk the Tier B block was placed **before** Task 5's final
`cubins.drain(...)`, so that drain stays literally the last thing before
`drainer.stop()` and its comment ("a final pass for anything the tick did not
reach") stays true. The two paths do not interact — the Tier B block touches no
cubin state.

The mechanical check: `git diff origin/main -- shim/stub/stub.cc` is **purely
additive, zero `-` lines**. Not one line of Task 5's stub work was displaced.

---

## 4. `shim/nvidia/cupti_adapter.cc` — the delicate one

Seven conflicts. Both tasks edit `on_callback`, `report`, `on_tick`,
`at_exit_handler` and `InitializeInjection`, and both insert a large block at
the same anchor (`bool g_names_was_attached = false;`).

| # | where | resolution |
| --- | --- | --- |
| 1 | `#include <cupti_pcsampling.h>` | both sides add the same include; kept once. Task 5's comment ("the ONLY thing this adapter uses from this header today; Task 6 uses the rest") was rewritten — Task 6 *is* here now, so the comment had become false. |
| 2 | emitter block | union: `gpu_module_load_v1` + the four PC-sampling probes |
| 3 | the big block (658 lines) | union: Task 5's `// module path` section (`capture_enabled`, `cupti_cubin_crc`, `on_cubin_captured`, `on_module_loaded`) first, then Task 6's forward decls and the whole `// PC sampling (Tier B)` section. The `}` that followed the conflict closed `on_module_loaded` on one side and `on_finalize` on the other, so a `}` was reinstated between them. |
| 4 | `on_callback`, RESOURCE domain | **both handlers, sequentially** — see below |
| 5 | `report()` | merged cleanly; both counter sets verified present |
| 6 | `on_tick` | **ordering hazard — see below** |
| 7 | `at_exit_handler` | union: `on_finalize("exit")`, `cuptiActivityFlushAll`, `lb`/`eb` flush, `g_pcb->flush()`, then Task 5's final cubin drain, then `report("exit")` |
| 8 | `InitializeInjection` | union: Task 5's `g_cubins` / `g_cubin_timeout_ms` / `g_consumer_enrolled` / `cubin_name`, then Task 6's `g_replay` and the whole Tier B setup block |

### 4a. The `MODULE_UNLOAD_STARTING` drain — the one to be most careful with

`cupti_pcsampling.h` requires a PC-data flush after every module
load-unload-load in `CONTINUOUS` mode to keep PCs unique, and missing it does
not lose data — it makes two different instructions share a PC identity,
silently, with nothing counted anywhere. Task 5 edits the same domain's
callback, so this is exactly where the drain could have been lost.

Both sides rewrote the `CUPTI_CB_DOMAIN_RESOURCE` branch of `on_callback`.
Task 5 added an `if (cbid == CUPTI_CBID_RESOURCE_MODULE_LOADED)` read; Task 6
added a call to `on_resource(cbid, …)`, whose `switch` owns
`CONTEXT_CREATED`, `CONTEXT_DESTROY_STARTING` and `MODULE_UNLOAD_STARTING`.
Resolved by running **both, sequentially, for every cbid** — deliberately not
as an `if/else`, which is the shape that would have silently dropped one:

```c
if (cbid == CUPTI_CBID_RESOURCE_MODULE_LOADED && cbdata)
    on_module_loaded((const CUpti_ResourceData *)cbdata);
on_resource(cbid, (const CUpti_ResourceData *)cbdata);
return;
```

They cannot shadow each other: `on_module_loaded` reacts only to
`MODULE_LOADED`, and `on_resource`'s `switch` has no `MODULE_LOADED` case, so
that cbid hits its `default: break`. The reason is now written into the code at
that site, so a future edit that tries to make it an `else` has to read why it
is not one.

**How the drain was confirmed to survive, concretely:**

1. **Line-level, mechanically.** Every added line of Task 6's diff against the
   fork point was checked for presence in the merged file:
   `diff -u base.cc t6.cc | grep '^+'` → **0 of 946 lines missing**. The same
   check for Task 5 → 2 lines missing, and both are the include comment that
   was intentionally rewritten. Task 6 deleted nothing from the base; Task 5's
   only deletions are the two `logf` lines it replaced, and the merged file
   carries the replacement and not the originals.
2. **Structurally.** The `MODULE_UNLOAD_STARTING` case is present in
   `on_resource`, still calls `pc_drain_all(PCDrainReason::kModuleUnload)`
   before returning, and still **blocks** on `g_pc_mu` (`pc_drain_all` takes
   `std::lock_guard`) rather than trying the lock — which is the whole point:
   `on_finalize` is the only path that uses `try_lock`, and it still does.
3. **Reachably.** `on_resource` is called from `on_callback` for every RESOURCE
   cbid, and `cuptiEnableDomain(1, …, CUPTI_CB_DOMAIN_RESOURCE)` is still
   unconditional in `InitializeInjection`.
4. **Compiled.** `make -C shim nvidia` builds the merged adapter against real
   CUDA 13.3 headers and links real `libcupti`, exit 0.
5. **Scheduled.** `core/pcdrain_test.cc` — which proves forced (unload) drains
   ignore the phase, that five unloads inside one period produce five drains,
   and that a forced drain resets the phase — runs and passes in
   `make -C shim test`.

### 4b. `on_tick` — the ordering hazard, and the one place a textual union would have been wrong

Task 6's `on_tick` addition contains `if (!g_pc_tier_b) return;`. Task 5's
addition is the cubin `drain()` — the **send half** of every module capture.
Tier B is **off by default**. So appending Task 6's block ahead of Task 5's, as
a naive union or a "theirs-first" resolution would, would have left cubin
offers unreachable in the default configuration: `modules_captured` climbing,
`cubins_sent` stuck at zero, every module unresolvable, and no counter anywhere
reading wrong.

Resolved with the cubin drain **above** the Tier B gate, and the constraint
written into the comment at that site so it cannot be reintroduced by a later
edit. Task 6's own ordering choice is preserved too: the `classGraphExec` drop
emission still sits *before* `if (!g_pc_tier_b) return;`, because that
condition belongs to the launch→execution join and is exactly as invisible with
PC sampling off as on.

### 4c. The `CubinView` guard

Untouched and verified. `shim/core/cubinqueue.h` was not a conflicted file and
has no diff against `main`. `on_module_loaded` still constructs
`const perfagent::CubinView view(m->pCubin, m->cubinSize)` and passes it
straight into `g_cubins->capture(...)`, with `moduleId` travelling as an opaque
context rather than a captured pointer — the copy-in-the-callback,
offer-on-the-drain-thread contract `core/cubin.h` states, intact.

The structural proof still holds:

```
$ make -C shim check-cubin-defer
check-cubin-defer: OK - the compliant capture compiles, all 5 deferrals do not
```

It also still runs as a prerequisite of `make -C shim test`, which is what
keeps it from becoming a target nobody invokes.

### 4d. Both counter sets in `report()`

`report()` merged without a conflict, which is the case worth double-checking
rather than trusting, because Task 6 adds an early `return` when Tier B is off.
Verified by inspection: Task 5's eleven cubin counters are in the *main* `logf`
format string (line ~1202), which is emitted before the resource-cbid
histogram, the `graph_execs`/`multi_device` line and Task 6's
`if (!g_pc_tier_b) { … return; }`. A Tier-B-off run still prints every cubin
counter.

All nine probes are present in the built adapter — Task 5's and Task 6's
together — and it still exports exactly one dynamic symbol:

```
$ readelf -n shim/libperfagent-gpu-nvidia.so | grep 'Name: gpu_'
  gpu_launch_v1  gpu_exec_v1  gpu_pc_sample_batch_v1  gpu_dropped_v1
  gpu_config_v1  gpu_stall_reason_map_v1  gpu_kernel_name_v1
  gpu_module_load_v1  gpu_launch_sampled_v1
$ nm -D --defined-only shim/libperfagent-gpu-nvidia.so
0000000000003cb0 T InitializeInjection
```

---

## The re-derived bounds

`make generate` is idempotent — run twice, the second run leaves the tree
clean — and the regenerated objects are **byte-identical to Task 6's committed
`.o`**, so the rebase changed nothing in the BPF program. `KIND_MAX` stays 16,
no map layout moved.

From `llvm-objdump -d gpuprobe/gpuusdt_x86_bpfel.o`:

```
  95:  b7 02 00 00 28 0c 00 00   r2 = 0xc28      ; reserve 3112, into bpf_ringbuf_reserve
  97:  85 00 00 00 83 00 00 00   call 0x83
 101:  b7 01 00 00 00 0c 00 00   r1 = 0xc00      ; clamp 3072
 102:  2d 71 01 00 00 00 00 00   if r1 > r7 goto +0x1
 103:  b7 07 00 00 00 0c 00 00   r7 = 0xc00
 119:  07 01 00 00 28 00 00 00   r1 += 0x28      ; payload at header + 40
 122:  85 00 00 00 70 00 00 00   call 0x70       ; probe_read_user(dst, clamped_len, src)
```

| bound | value | hex | where |
| --- | --- | --- | --- |
| reserve | **3112** | `0xc28` | insn 95, the `bpf_ringbuf_reserve` size |
| clamp | **3072** | `0xc00` | insns 101–103, the clamp-to-max on the payload byte count |
| payload offset | **+40** | `0x28` | insn 119, the reserved pointer advanced past `struct batch_hdr` |

3072 + 40 = 3112, and `batchHdrSize = 40` in `gpuprobe/consumer.go` pins the Go
side of the same number. Unchanged, as they have been across every BPF change
in this program.

---

## Verification actually run

```
make generate (x2)                  idempotent; gpuprobe .o byte-identical to Task 6's
make -C shim                        exit 0
make -C shim test                   exit 0  (check-cubin-defer OK, pcdrain_test OK,
                                             cubinqueue_test OK, probe_order_test OK)
make -C shim check-fpless           OK
make -C shim check-cubin-defer      OK - the compliant capture compiles, all 5 deferrals do not
make -C shim nvidia                 exit 0  (CUDA 13.3, real libcupti)
go build ./... && go vet ./...      clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1
                                    ok gpu 3.7s, gpuprobe 4.6s, 10 internal pkgs
go test ./gpuprobe/ -race -count=4  ok 18.6s
~/go/bin/golangci-lint run          0 issues
```

`CapEff: 0`, no GPU: the hardware gates skip, and no `CUpti_` code path in this
commit has executed here. Everything in Task 6's "cannot verify" list is still
outstanding and unchanged by this rebase.

**One thing deliberately not committed.** `make generate` also rewrites
`cpu/`, `offcpu/` and `profile/`'s `.o` files, which differ from what is
committed on `main`. That is pre-existing local toolchain drift in packages this
branch does not touch; those files were restored so the commit stays scoped to
the GPU work.
