# Issue #93 — the cubin transport now feeds `gpu.ModuleStore`

Branch `fix/wire-cubins-to-modulestore`, one commit, on top of `e09c073a` (merge of #92,
`feat/phase6-gate`). Verified on this machine: `CapEff: 0`, no GPU, no passwordless sudo
(`sudo -n true` → "a password is required"). What that costs is stated in "What ran and
what only compiled" rather than glossed.

Nothing in `shim/`, `bpf/` or `internal/` changed. The transport, its seal verification,
its admission bucket and the enrolment isolation are byte-for-byte untouched:
`git diff --stat` shows `gpuprobe/cubin.go` +66/-7, all of it new code beside the
existing path, and no line of `handle` altered.

---

## 1. The wiring

Four objects, one instance, three readers.

| where | what it does with the store |
| --- | --- |
| `gpuprobe.Config.Modules` (new) | the store the cubin listener writes every accepted cubin into |
| `gpu.TimelineConfig.Modules` (existing) | the Snapshot join resolves a pending PC group's `(crc, functionIndex)` to a device function name through it |
| `gpu.ProjectionConfig.Modules` (existing) | `setSourceLabels` resolves `gpu_src_*` against it |
| the driver (`cmd/gpu-cuda-profile`, `cmd/gpu-stub-profile`) | constructs it, hands the **same pointer** to all three |

Three pieces of code:

**`gpu.ModuleStore.Has(crc) bool`** — live membership, refreshing LRU recency. The
transport asks before it maps a payload, so the answer decides whether a memfd is mapped,
copied and parsed again for bytes already held. It refreshes recency for the same reason
`Put`'s already-held path does (a re-offer is the producer saying the module is live), and
it increments none of the `Resolve*` counters, so Task 4's "the four partition every
`Resolve` call" identity is intact. That identity is asserted in the new test.

**`gpuprobe.moduleStoreSink`** — the adapter, holding `*gpu.ModuleStore` and satisfying
`cubinSink`. An adapter rather than methods on `gpu.ModuleStore` because the two contracts
differ in one place (below) and because `gpu` should not grow a method named for this
transport.

**`gpuprobe.cubinSinkFor(cfg)`** — `Config.Modules` when the caller supplied one, the
bounded `memCubinStore` when it did not. `Attach` still calls `newCubinListener(cfg, nil)`;
the nil now means "the sink `cfg` asks for" rather than "the placeholder". An explicit sink
remains a test seam and nothing else.

`gpu.ModuleStore` itself is otherwise unchanged: no new branch in `Resolve`, no new status,
no memoized resolution. A nil store still yields `no-module` by being answered by an empty
store inside `ProjectExecutionsWith`, exactly as before — the projection still does not
decide the enum.

### The one place the two contracts disagree, and where it is resolved

`gpu.ModuleStore.Put`'s error is **diagnostic**: unparseable bytes are still stored (so a
re-offer is not re-parsed), counted in `ModulesUnparseable`, and resolve as `no-module`.
`cubinSink.PutCubin`'s error means "the offer did not land", and the listener answers it
with `reply('X')` and `CubinsRejectedMalformed++`.

Propagating one as the other would have the transport report a rejection for a cubin the
agent is holding: `CubinsRejectedMalformed` non-zero on a healthy run, `CubinsReceived` and
`CubinBytesReceived` understating what is held (and the total-bytes ceiling with them), and
the producer told `'X'` for an offer that landed and that `HasCubin` will answer `true` for
a millisecond later. So `PutCubin` reports only a failure to *store*, and the two facts
stay in the two counters where each is true — the transport counts what arrived, the store
counts what it can read. `Put` has no other error today; if it grows one, the adapter
returns it.

This is pinned by `TestACubinTheStoreCannotParseStillLandsAndIsCountedApart`, and the
mutation that propagates the error fails it by name.

---

## 2. What the drivers configure, and why

Both drivers build the store with `gpu.NewModuleStore(gpu.ModuleStoreConfig{})` — the
package defaults, **512 modules and 64 MiB** — taken rather than restated.

- **512 modules** is already the JIT/template-explosion case rather than a normal
  workload; a real process has one cubin per loaded module, tens to low hundreds.
- **64 MiB** is the tighter of the two resident bounds in play. The transport's own
  ceilings sit *outside* it: 8 MiB per cubin (`Config.CubinMaxBytes`) and 256 MiB total
  offered (`Config.CubinTotalBytes`). So the store is what actually caps what this process
  holds, and neither driver has a workload-specific reason to deviate.
- Writing the numbers again at each call site would put a second copy of the sizing where
  drift is invisible. The rationale is at the call site; the numbers are in one place.

Both bounds evict least-recently-used, and eviction is honest — see §4.

Both drivers also now log `store.Stats()` after `JoinHealthWith`. That line is the one that
separates the two ways a profile full of `no-module` can happen: `modules_stored=0` means
nothing arrived (look at the `Cubins*` counters), while `modules_stored>0` with
`resolve_no_module` high means the CRCs the PC records join on are not the CRCs the cubins
arrived under — which is hardware assertion 13, and the single most consequential thing the
first GPU run has to check.

`cmd/gpu-stub-profile` is wired identically. It selects no tier of its own, but the stub
inherits `PERFAGENT_GPU_PC_SAMPLING` and `PERFAGENT_STUB_CUBINS` from the operator's
environment, so a run configured that way now produces real source labels instead of
`no-module`, and an unconfigured run holds an empty store.

---

## 3. The gate assertion: from pin to real

Task 13 pinned this gap with `TestGateTheCubinTransportDoesNotYetFeedTheModuleStore`, which
passed *because* the gap existed — it asserted that the listener's default sink was
`*memCubinStore` and that no `gpuprobe.Config` field named a module store. Its own doc
comment said what to do when the hop was wired: delete it.

**It is deleted.** The gate slot it occupied now holds assertions, in
`gpuprobe/gate_compose_test.go`'s `TestPhase6GateConsumerHalf`:

| sub-test | what it asserts |
| --- | --- |
| `assertion-02-an-offered-cubin-becomes-a-source-line` | a cubin crosses the real socket as a real sealed memfd, the listener writes it into the store `Config.Modules` named (asserted `require.Same`, not merely "a store"), and a PC sample carrying that CRC comes out of `ProjectExecutionsWith` as `gpu_src_status="resolved"` with `gpu_src_file="single.cu"` and a line checked against `single.cu` itself, on a sample whose frames are the launching CPU call path |
| `assertion-02-an-evicted-module-is-no-module-and-can-return` | §4 |
| `assertion-02-an-unreadable-cubin-still-lands-and-is-counted-apart` | §1's contract seam |
| `assertion-04-no-store-is-a-supported-no-module-state` | a store-less consumer still binds, admits and counts offers, keeps the bounded placeholder, and resolves `no-module` |

and in `gpu/gate_test.go`'s `TestPhase6Gate`, under assertion 4:
`assertion-04-an-evicted-module-is-no-module-not-stale` (Task 4's own test, now composed
into the gate because eviction is reachable in production for the first time) and
`assertion-04-membership-is-live-and-counts-as-use`.

All of these **run unprivileged on this machine**. The producer in them is the test process
itself, offering the checked-in fixture over the real abstract socket as a real sealed
memfd — which is byte-for-byte what `shim/core/cubin.cc` does, pinned from the other side
by `TestTheCppProducerAndTheGoCubinListenerAgreeOnTheWire`.

### The privileged gate

`TestStubDrivesPCSamplingToPprofWithoutAGPU` no longer builds the store's contents. It
still constructs the store — as a driver does — but hands it to `gpuprobe.Config.Modules`
and then **asserts what the product put in it**:

```
require.Equal(t, 2, store.Len(), "the cubin transport did not feed the module store ...")
assert.Equal(t, uint64(1), ms.ModulesWithLineInfo)
assert.Equal(t, uint64(1), ms.ModulesWithoutLineInfo)
assert.Equal(t, gpu.SrcResolved,   store.Resolve(gateCRCLineInfo,  lineIdx,  0x10).Status())
assert.Equal(t, gpu.SrcNoLineInfo, store.Resolve(gateCRCNoLineInfo, noLineIdx, 0x10).Status())
```

The two `store.Put(...)` calls that filled it are gone. The CRCs are still the ones the
**producer** declared (parsed from its own report and independently recomputed in Go over
the fixture bytes), so the identity the store is keyed on is still the identity that went
on the wire — that has not been weakened, it has been moved from "the test puts it there"
to "the test checks it arrived there".

`TestStubDrivesThePipelineToPprofWithoutAGPU`, the baseline gate, is untouched.

---

## 4. Is assertion 2 now reachable through the producer?

**Half of it is, and the missing half is a second hop that is not in this product.** Stated
precisely, because "route around it" was not an option here.

Assertion 2 is the conjunction of a real CPU stack and `gpu_src_status="resolved"` naming a
real line of the fixture's source. It has two producer-side inputs:

1. **the cubin, and the store it must reach.** This is what #93 was, and it is now driven
   entirely through the shipping path: the producer sends, the transport receives, the
   listener writes `Config.Modules`, the Timeline and the projection read that same
   instance. Nothing in any test puts bytes into the store any more.
2. **a PC record that reaches an execution.** This is **still injected** at
   `Timeline.EmitPCSample` in the privileged gate, and it still has to be.

The reason is Task 13's finding 1, unchanged by this commit and still pinned by
`TestGateTheStubsPCRecordsCannotAttributeToAnything` (unprivileged, ran):

- `shim/stub/stub.cc` keys its PC records on `kStubCubinCRC = {0xC0FFEE01, 0xC0FFEE02}`,
  two compile-time constants, while the cubins the same run delivers are keyed by a
  content hash of the fixture bytes. No cubin is ever stored under a `0xC0FFEE0n` key, so
  the module lookup cannot fire whatever the store holds;
- their `correlation` is 0 in every tier, so the exact-correlation path is unavailable;
- Tier B attribution runs `crc → module → function name → the execution's KernelName`, and
  the stub's kernel names are `kernel_1111`/`kernel_2222` while the fixtures' only function
  is `addOne`. No name can match.

So the wire path from a *stub* PC record to a source line is broken **in the stub**, one
hop upstream of anything this issue touched. Fixing it is a change to `shim/stub/stub.cc`
(record the CRC each capture computed; name the kernels after the fixtures' functions),
which is finding 1's fix and a separate change — `shim/` is also live on another worktree
right now. It is a finding, reported, not routed around.

**What this means for the phase.** Gate assertion 14 — a flame graph reaching a real line
of `cuda_workload.cu` from a CPU stack on the RTX 3090 — was blocked by #93 *and* is not
blocked by finding 1: the real CUPTI adapter emits real CRCs and real kernel names, so on
hardware the join has both halves. The stub's inability to drive it is a property of the
stub, not of the product. What the unprivileged gate now proves is the whole chain minus
the BPF decode of `KIND_PC` (asserted separately and exactly by the 64 wire records in the
privileged gate): socket → seals → store → module join → projection → pprof labels.

---

## 5. The eviction guarantee, and why the wiring cannot bypass it

Task 4's `TestResolveAfterEvictionIsNoModuleNotStale` pins that an evicted module answers
`no-module` and never the line it used to have. The store has no memoized resolution, so
that half is absolute.

The wiring has exactly **one** way to break it, and it would look like an optimisation: the
transport asks `HasCubin` before mapping and treats "yes" as a counted no-op, so a set of
"CRCs seen once" kept anywhere on this path — in the adapter, in the listener — would make
one eviction permanent. The module would never be re-admitted, every PC sample for it would
read `no-module` forever, and `CubinsDuplicate` would climb while every store counter read
healthy. That is this project's recurring defect class exactly.

So `HasCubin` is `store.Has`, live membership and nothing else, and
`TestAnEvictedModuleAnswersNoModuleAndIsOfferedAgainNotSuppressed` drives it over the real
socket: offer, resolve, re-offer (counted duplicate, **nothing mapped**), evict under
`Capacity: 1`, assert `no-module`, assert `HasCubin` is false, re-offer, assert it is stored
again (`received` 2 → 3, `duplicate` still 1) and resolves to the **same line** it did
before.

### Mutation checks (each applied, run, reverted)

| mutation | result |
| --- | --- |
| `cubinSinkFor` ignores `cfg.Modules` (the pre-#93 behaviour) | gate assertions 02-source-line, 02-evicted, 02-unreadable FAIL |
| the adapter keeps a `sync.Map` of CRCs it has seen | assertion 02-evicted FAILS: "the wiring remembers a CRC the store has evicted; one eviction would then be permanent" |
| `PutCubin` returns `Put`'s diagnostic error | assertion 02-unreadable FAILS: "the offer was refused; the bytes crossed and are held, so the transport must say so" |

---

## 6. A finding: the transport's total ceiling is now a lifetime budget, not a resident one

Not fixed here, because fixing it means changing the transport, which this task was told not
to do. Stated so it is on the record.

`cubinListener.handle` charges the total-bytes ceiling against `l.stats.bytes`, which is a
**cumulative** count of everything ever received. With the old placeholder store nothing was
ever evicted, so cumulative and resident were the same number and `CubinTotalBytes` bounded
both. With `gpu.ModuleStore` they diverge: the store evicts at 64 MiB resident while the
listener keeps counting toward 256 MiB cumulative, and a re-offer after an eviction is
charged again.

The consequence for a process that loads more than 256 MiB of *distinct* cubin bytes over
one run: the channel starts refusing every further offer with
`CubinsRejectedTooLarge` ("total ceiling"), and modules loaded after that point resolve
`no-module`. It is bounded, counted, and visible in the reason string — it is not silent —
but the counter's meaning has shifted from "we are holding too much" to "we have been sent
too much", and the two are no longer the same. The 512-module placeholder had a comparable
cliff (offers past 512 were refused as store failures), so this is not a regression, but it
is now the *only* cliff and it deserves its own issue: the listener should charge the
ceiling against what the sink actually holds, or the two ceilings should be documented as
lifetime-vs-resident.

---

## 7. What ran and what only compiled

**Ran, unprivileged, on this machine:**

```
make -C shim && make -C shim test && make -C shim check-fpless \
  && make -C shim check-cubin-defer && make -C shim nvidia     # all OK
go build ./... && go vet ./...                                  # clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1              # ok
go test ./... -count=1                                          # ok (whole repo)
go test ./gpu/ ./gpuprobe/ -race -count=4                       # ok (26.5s / 20.3s)
~/go/bin/golangci-lint run --timeout=5m                         # 0 issues
```

Every assertion in §3's table **ran**, including the full socket → store → Timeline →
projection → pprof-label chain, plus `TestPhase6Gate` and `TestPhase6GateConsumerHalf` in
their entirety.

**Compiled only:** `TestStubDrivesPCSamplingToPprofWithoutAGPU` — the privileged end-to-end
gate, including the new `store.Len() == 2` / `ModulesWithLineInfo == 1` /
`Resolve(...) == SrcResolved` block that is issue #93's own assertion. It type-checks, vets
and lints clean and skips here with the message naming its setcap line. `CapEff: 0` and
`sudo -n true` refuses, so it cannot be setcap'd or run on this machine at all.

**Cannot verify.** Explicitly, in the order of how much it would matter:

1. **That the wiring works on hardware.** Nothing here has seen a cubin CUPTI produced.
   Everything moved is a checked-in `sm_86` fixture over a socket in one process. The
   privileged gate is the closest thing and it did not run.
2. **That `cuptiGetCubinCrc()` over the received bytes equals the `cubinCrc` the PC records
   carry** — Task 3's outstanding item, and now the *only* thing between this wiring and a
   resolved source line on the RTX 3090. If the two numbers disagree on hardware, every PC
   sample resolves `no-module` with every counter reading green, and the new
   `module store: {...}` log line in both drivers is what tells the two cases apart
   (`modules_stored=2, resolve_no_module` high).
3. **That `functionIndex` is the cubin's `.symtab` index** — unchanged premise, Task 6
   measures it. Every test here reads the index out of the fixture and asserts only that
   the store and the sample use the same one.
4. **Whether the store's defaults are the right size for a real workload.** 512 / 64 MiB
   is reasoning, not measurement; no run has ever pressured them. `ModulesEvicted*` in the
   new driver log line is what would say so.
5. Finding 1 above: whether assertion 2 is drivable from the *stub* — it is not, and the
   fix is in `shim/stub/stub.cc`, out of scope here.
