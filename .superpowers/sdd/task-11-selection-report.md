# Task 11 — Tier selection

Branch `feat/tier-selection`, one commit on top of `feat/tier-a-serialized` (`d3dcb61f`, Task 10 /
PR #89, which is itself one commit on `main`). Rebase onto `main` when #89 merges.

No ABI change, no BPF change, no `.o` churn. Nothing in Tier A's or Tier B's behaviour, the cubin
capture, the `CubinView` guard or the `MODULE_UNLOAD_STARTING` drain was touched except to be
gated by the setting this task adds.

---

## The setting, and its three values

**One setting.** `PERFAGENT_GPU_PC_SAMPLING`, read by both producers through one parser
(`shim/core/pctier.h`), written by the agent from one parser (`gpu/tier.go`), and surfaced on
`cmd/gpu-cuda-profile` as `--gpu-pc-sampling`.

| value | tier | what it does |
| --- | --- | --- |
| `off` (default, and what unset means) | none | no PC-sampling call at all |
| `continuous` | Tier B, `CUPTI_..._CONTINUOUS` | no serialization; PC samples join through the module |
| `serialized` | Tier A, `..._KERNEL_SERIALIZED`, duty-cycled | exact launch attribution; **perturbs the workload** |

`0` / `1` / `2` are accepted as aliases on both ends and are not legacy debt to be dropped: this
variable has been numeric since Task 6, container specs are already set that way, and a parser
that ignored them would turn a configured Tier A run into a silent off one. The agent **writes**
the name, because `pc_sampling=serialized` in `ps eauxwww` or a pod spec is a value nobody has to
go look up.

**The zero value is off**, in Go (`PCSamplingOff = iota`) and in C++ (`PCSamplingTier::kOff = 0`).
This is the same discipline `SerializationUnknown` carries and it exists for the same reason: a
config nobody filled in, a struct a test built by hand, a field lost in a copy and a parse that
failed all land on the tier that does not touch the workload. Turning PC sampling **on** has to be
written somewhere, deliberately.

**Every refusal falls closed to `off` and says so at length. None of them picks a tier.** An
unreadable setting resolved to "the cheaper one" or "the first token" is a decision the operator
did not make and cannot see in the output.

`TimelineConfig.SerializedSampling bool` is **replaced** by `TimelineConfig.PCSampling
PCSamplingTier` rather than joined by it. One field, not a tier plus a bool: two fields that can
disagree about which tier is running is precisely how a profile ends up disclosing one thing and
doing another. The tier also rides out on `Snapshot.PCSampling`, because "Tier A was asked for and
no window arrived" (everything `"unknown"`) and "Tier A was never asked for" (everything
`"false"`) are different facts and an inference from `SamplingWindowsReceived == 0` gets exactly
that case backwards.

---

## Why exclusivity is process-wide

`COLLECTION_MODE` is a single **per-`CUcontext`** CUPTI attribute, so a process could in principle
set `KERNEL_SERIALIZED` on one context and `CONTINUOUS` on another. Nothing in CUPTI forbids it.

What forbids it is that **which context a given kernel lands on is the application's choice, not
the profiler's.** A "both" mode would therefore emit one profile in which some kernels carry exact
launch attribution and inflated durations while others carry inferred attribution and honest ones,
split along an axis the operator can neither see nor control. A profile whose trustworthiness
varies invisibly is worse than either tier alone.

So the selection is process-wide and exclusive, and **naming two tiers is a startup error**. It is
reachable in three shapes, all refused, all with the reason attached rather than only the rule:

1. **one value naming two tiers** — `continuous,serialized`, `1+2`, `serialized continuous`. The
   value is *parsed* rather than rejected as a syntax error on purpose: "both" has to be
   expressible for the refusal to be reachable at all, and a parser that quietly took the first
   token of `continuous,serialized` would be the silent pick this rule exists to prevent. Both
   orderings are tested, because such a parser answers "continuous" for one and "serialized" for
   the other — correct-looking in half the runs, perturbing the workload in the other half.
2. **the flag and the environment naming different tiers.** There genuinely are two sources: a
   driver takes a flag, and the producer's environment may already carry the variable from a shell
   export or a container spec. There is deliberately **no precedence** between them — resolving it
   by one would leave the profile's attribution quality decided by whichever source the operator
   forgot about. An *unspecified* flag defers to the environment (that is not a disagreement); an
   explicit `--gpu-pc-sampling=off` against an exported `serialized` is one, and is refused.
3. **an unknown value**, in either source, attributed to the source that carried it — an operator
   staring at a correct flag needs to be told the environment is what is wrong.

The producer refuses on its own account too, rather than trusting the agent to have filtered:
`shim/core/pctier.h` returns `kUnknown` / `kNotExclusive`, both adapter and stub log the whole
explanation, and both fall to `kOff`. The adapter counts it in `g_pc_tier_refused` and prints it —
a startup log line in somebody else's process is routinely swallowed by whatever captures its
stderr, and "PC sampling produced nothing" and "PC sampling was refused at startup" must not look
the same in the report.

**Switching tiers mid-run is out of scope**, gated on Task 10's open hardware question about
whether `COLLECTION_MODE` can change between `Stop` and `Start` without a full `Disable`/`Enable`.
Nothing here depends on the answer; the mode is set once, at context creation.

---

## The acknowledgement gate on `serialized`

Tier A is a destructive flag in the ordinary sense — it changes the thing being measured — so it
takes the same shape as one. `serialized` is refused unless
`--gpu-pc-sampling-acknowledge-perturbation` is set (`PCSamplingRequest.AcknowledgePerturbation`),
and the refusal names all three perturbations rather than just naming a flag, because that text is
the last thing an operator sees before deciding to pass it:

> GPU PC-sampling tier "serialized" perturbs the workload and was not acknowledged. Tier A
> serializes GPU kernels in bursts: it inflates the kernel durations this profile reports, it
> distorts any CPU and off-CPU profile taken alongside it with no marking in those profiles at
> all, and it is unavailable where CUDA graphs are in use. Re-run with
> `--gpu-pc-sampling-acknowledge-perturbation` if that is what you want

An unacknowledged Tier A does **not** run, and does not run as Tier B either — a silent downgrade
would leave the operator reading a Tier B profile believing they asked for Tier A. The
acknowledgement gates Tier A and nothing else: `off` and `continuous` behave identically with and
without it, so an operator who leaves it set in a script has not thereby changed what those do.

---

## The standing warning, verbatim

`gpu.PCSamplingStandingWarning(tier)` returns these four lines for `PCSamplingSerialized` and
`nil` for the other two tiers. `JoinHealthWith` emits them on **every** render, immediately under
the summary and above the anomalies — a perturbation notice shown once at startup has scrolled
away long before the profile it applies to is read. `cmd/gpu-cuda-profile` also prints them at
startup, for the operator watching the run begin; the standing copy is for the one reading the
profile an hour later, who is the reader the warning is actually for.

```
gpu pc sampling WARNING: Tier A ("serialized", CUPTI KERNEL_SERIALIZED) PC sampling was selected for this run — the profiler deliberately perturbs the workload it is measuring. This warning stands for the whole run, not just at startup. It names three distinct perturbations, because an operator told only about the first will misread the other two.
gpu pc sampling WARNING: (1) GPU KERNEL DURATIONS INSIDE A BURST ARE INFLATED by serialization — kernels that would have overlapped ran one at a time. Those executions are marked gpu_serialized="true"; executions that cannot be shown to have run outside every burst are marked "unknown" and must never be read as "false".
gpu pc sampling WARNING: (2) CPU AND OFF-CPU SAMPLES TAKEN DURING A BURST ARE DISTORTED AND CARRY NO MARKING AT ALL. gpu_serialized reaches only the GPU projection (ProjectExecutions); the on-CPU and off-CPU profilers are a separate path with no window awareness, and serialization inflates precisely the synchronization wait that off-CPU profiling exists to measure. cudaDeviceSynchronize-shaped off-CPU time will look worse than it is, with nothing in that profile saying why.
gpu pc sampling WARNING: (3) TIER A IS UNAVAILABLE WHERE CUDA GRAPHS ARE IN USE. A graph launch fires one runtime callback for N kernels, so Tier A's exact-launch attribution would be false while still looking exact; the producer refuses to open bursts in such a process rather than downgrading silently to Tier B, and this profile then carries no Tier A PC samples at all.
```

**All three, because an operator told only about the first will misread the other two.** They will
see `gpu_serialized="true"` on the GPU samples, conclude that the marked ones are the perturbed
ones, and then trust an off-CPU profile whose synchronization waits are inflated by exactly this
mechanism and carry no marking at all. A limitation an operator is told about is a limitation; one
they discover from a misleading profile is a defect.

**It stands even when nothing went wrong.** `TestTheTierAWarningStandsOnAnOtherwisePerfectRun`
renders it on a Tier A snapshot with every join exact, no window and no serialized execution —
which is a real state (a burst-free interval, a graph refusal that stopped bursts, the first
snapshot of a run), and is exactly the profile whose reader has no other way to learn the tier was
on. A disclosure that appeared only once some counter moved would be absent from precisely those.

**It is not an anomaly, and it is not counted as one.** The summary gained its own clause and the
identity is now `len(lines) - 1 == warnings + anomalies`, asserted over four snapshots:

```
gpu join: 512 executions, all exact; 256 launches, all matched; cache 256 live; pc sampling serialized; 4 standing warning lines; no anomalies
```

`anomalousSnapshot()` now sets `PCSampling: PCSamplingSerialized`. It has to: the three
serialization counters it carries are reachable **only** under Tier A, so the fixture as it stood
described a run that cannot happen — and would quietly have stopped exercising the warning that a
real one carries.

**One anomaly was strengthened while in there.** The `"unknown"` clause was guarded by
`&& SamplingWindowsReceived > 0`, which suppressed it in the worst available case: Tier A
selected, not one window record received, so *every* execution is `"unknown"`. Unknown executions
are unreachable in the other two tiers, so the guard bought nothing and hid the case that most
needed raising. The clause now names which cause applies instead of disappearing:

```
gpu join ANOMALY: 512 of 512 executions cannot be said to have run unperturbed — no sampling window covers them (NOT ONE window record reached the agent, though Tier A was selected — the producer never bursted, the probe never attached, or every batch was lost). They are marked gpu_serialized="unknown" and MUST NOT be read as "false"
```

---

## "off" means off, asserted at the probe site

The claim is about what leaves the producer, and every cheaper way of checking it checks something
else. A counter says what the producer *thinks* it emitted. A consumer-side assertion says what
survived a ringbuf. Reading the env in a unit test says what the parser returned. Seventeen
defects on this project have been counters and checks reading green exactly when things were
worst, so the assertion is made **where a uprobe would make it**.

`shim/stub/pc_tier_test.cc` reads its own `.note.stapsdt`, patches the probe nops with `int3` —
which is all a uprobe does — and counts the traps in a `SIGTRAP` handler. No `CAP_BPF`, no
consumer, no GPU. It traps the four PC-sampling probes (`gpu_pc_sample_batch_v1`,
`gpu_stall_reason_map_v1`, `gpu_config_v1`, `gpu_sampling_window_v1`) **and** `gpu_launch_v1` /
`gpu_exec_v1` as controls, arms every semaphore, and leaves the stub's own PC knobs turned **up**
(`PERFAGENT_STUB_PC_SAMPLES=128`, `PERFAGENT_STUB_SAMPLING_WINDOWS=4`) for every pass — the tier
must silence the producer while everything else is asking it to speak.

```
pc_tier_test: tier unset ok - pc_sample=0 stall_map=0 config=0 window=0 (launch=2 exec=2)
pc_tier_test: tier=off ok - pc_sample=0 stall_map=0 config=0 window=0 (launch=2 exec=2)
pc_tier_test: tier=0 ok - pc_sample=0 stall_map=0 config=0 window=0 (launch=2 exec=2)
pc_tier_test: tier=continuous ok - pc_sample=4 stall_map=8 config=1 window=0 (launch=2 exec=2)
pc_tier_test: tier=serialized ok - pc_sample=4 stall_map=8 config=1 window=8 (launch=2 exec=2)
pc_tier_test: tier=nonsense ok - pc_sample=0 stall_map=0 config=0 window=0 (launch=2 exec=2)
pc_tier_test: tier=continuous,serialized ok - pc_sample=0 stall_map=0 config=0 window=0 (launch=2 exec=2)
pc_tier_test: tier=serialized,continuous ok - pc_sample=0 stall_map=0 config=0 window=0 (launch=2 exec=2)
```

Non-vacuity is the other half and is asserted three ways, because an "off" pass that trapped
nothing would be equally green if the producer had simply failed to run: the launch and exec
probes must fire on **every** pass including the off ones; the `continuous` pass must fire the
sample, stall and config probes, proving those sites are reachable in this very binary; and the
`serialized` pass must fire the window probe. Note also that `continuous` fires **no** window
record: a window is Tier A's own disclosure, and a `CONTINUOUS` producer announcing one would be
claiming a perturbation it did not cause.

**The test was mutation-checked.** Removing the tier gate from `stub.cc`'s `pc_samples` makes the
first three passes fail with `gpu_pc_sample_batch_v1 FIRED 4 times (128 records) with the tier
off`. It is a check that can go red.

This required the stub to honour `PERFAGENT_GPU_PC_SAMPLING` at all, which it did not: its PC
emission was gated only on its own knobs. The stub is the producer the agent hands its selection
to on a machine with no GPU, so "off means off" is only assertable there if it obeys the same
setting the adapter does, from the same parser. The tier is now the **outer** gate and the stub
knobs the inner one. No existing test set those knobs, so nothing on the wire changed for any
current gate.

The agent half is narrower and still load-bearing:
`TestOffIsHandedToTheProducerExplicitly` pins that `off` is written to the child's environment
**explicitly** on every run, and `cmd/gpu-cuda-profile` appends it after `os.Environ()`. Whatever
an operator exported into their shell last week must not reach the producer of a run this agent
believes is off.

---

## What else is pinned

- `TestTheShimAndTheAgentAgreeOnTheTierSpellings` reads `shim/core/pctier.h` from the Go test and
  asserts the two parsers accept the same six spellings. A spelling only one end knows is a
  setting that silently does nothing — for `serialized` that hands the operator an unperturbed
  profile they will read as a perturbed one; for `off` it would be the opposite.
- `core/pctier_test.cc` — the producer parser: the three values in both spellings, case and
  whitespace, the null and empty settings, one tier named twice (redundant, not contradictory),
  both-tier orderings, the near-misses an operator actually types (`on`, `true`, `3`,
  `serialised`), a good token beside a bad one, the offending text reaching the log, a token
  longer than the log buffer truncating rather than overrunning it (this code runs inside somebody
  else's process), a null buffer, and `pc_tier_name` on an out-of-range value rendering `invalid`
  rather than `off`.
- `gpu/tier_test.go` — the three values, both both-set shapes, the unknown-value error, the
  acknowledgement gate from both sources, source attribution, the JSON round trip by name, and
  that only `PCSamplingSerialized` consults the window store while the other two still **ingest
  and count** windows that arrive anyway.
- The sum identity and every Task 10 assertion still hold; `assertSumIdentity` runs in the new
  timeline tests too.

## Counters and where each refusal is visible

| condition | agent | producer |
| --- | --- | --- |
| unknown value | startup error, `ErrPCSamplingUnknownTier` | log + `g_pc_tier_refused` (adapter), log (stub) |
| two tiers named | startup error, `ErrPCSamplingTiersExclusive` | log + `g_pc_tier_refused`, stub log |
| flag vs env disagree | startup error, `ErrPCSamplingTiersExclusive` | n/a — the agent never writes a conflicting value |
| `serialized` unacknowledged | startup error, `ErrPCSamplingNotAcknowledged` | n/a — never launched |
| tier in force | `Snapshot.PCSampling`, `joinhealth` summary clause | adapter `pc … tier=<name>`, stub `pc_sampling=<name>` |

The stub's `pc_sampling=` line is printed on **every** run including an off one, for the reason
that file already states about its cubin counters: a line that appears only on the interesting
runs is a line nobody checks on the boring ones, and the boring one is exactly where "off did not
mean off" would hide.

---

## Verification actually run

```
make -C shim                      exit 0
make -C shim test                 exit 0  (pctier_test ok, pc_tier_test 8/8 ok, plus every
                                           pre-existing test: burst, pcdrain, cubinqueue,
                                           probe_order, probe_args, usdt_abi, sampler, enroll)
make -C shim check-fpless         OK
make -C shim check-cubin-defer    OK - the compliant capture compiles, all 5 deferrals do not
make -C shim nvidia               exit 0 (CUDA 13.3, real libcupti)
go build ./... && go vet ./...    clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1     all pass
go test ./gpu/ ./gpuprobe/ -race -count=4              ok gpu 21.2s, gpuprobe 19.8s
~/go/bin/golangci-lint run --timeout=5m                0 issues
```

Plus the mutation check described above: with the stub's tier gate removed, `pc_tier_test` fails
on the three off passes and names the probe that fired.

---

## Cannot verify

`CapEff: 0`, no GPU on this machine, and **no `CUpti_` code path added or changed by this commit
has ever run.**

The plan says of this task: *"Must be measured on the RTX 3090 afterwards: nothing beyond what
Tasks 6 and 10 already cover."* That remains true — this task adds no CUPTI call. What it does add
to the hardware list is small and worth stating rather than assuming:

1. **That the adapter's tier parse runs before any `CONTEXT_CREATED` callback can reach the PC
   path.** It replaces an `env_uint` call in the same position, before `cuptiSubscribe`, so this
   is believed safe by construction for the same reason Task 6 item 17 was — and unverified in
   practice for the same reason.
2. **That `g_pc_tier_refused` is reachable on hardware.** It is reachable in the stub (three
   passes of `pc_tier_test` exercise the same parser through the same shapes), and the adapter's
   arm is a copy of it, but the adapter's report line has not been printed on a real run.
3. **That the agent's explicit `PERFAGENT_GPU_PC_SAMPLING=off` actually wins in the injected
   process.** `os/exec` deduplicates its environment keeping the last occurrence, and the value is
   appended after `os.Environ()`, so an inherited export is overridden — asserted by reading the
   Go standard library's behaviour, not by observing a CUDA process's `/proc/<pid>/environ`.
4. **Everything Tasks 6 and 10 could not verify** remains unverified; in particular Task 10's item
   3, whether `COLLECTION_MODE` can change between `Stop` and `Start`, which is the gate on
   mid-run tier switching ever being possible. This task is written so that the answer changes
   nothing here if it is "no".

Not verifiable at all, on hardware or otherwise, and unchanged by this task: MPS and cross-process
contention for the sampling hardware.
