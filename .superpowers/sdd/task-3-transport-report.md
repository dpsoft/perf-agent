# Task 3 — Cubin transport: a channel of its own

Branch `feat/cubin-transport`, one commit. Verified offline: `CapEff: 0`, no GPU,
no CUDA toolkit, no BPF. Everything the plan asked Task 3 to prove without
hardware is proven; the two items it deferred to the RTX 3090 are stated as
outstanding at the end and nothing here pretends otherwise.

## What was built

| file | what |
| --- | --- |
| `gpuprobe/cubin.go` | the consumer half: address, listener, admission bucket, seal verification, ceilings, counters, placeholder store |
| `gpuprobe/cubin_test.go` | 22 tests, 37 leaf cases, including the isolation test in both orders |
| `shim/core/cubin.h`, `shim/core/cubin.cc` | the producer half: sealed memfd, `SCM_RIGHTS` handover, one-deadline budget, two shim counters |
| `shim/core/cubin_test.cc` | the producer's own wire assertions |
| `gpuprobe/consumer.go` | `Config.CubinMaxBytes` / `CubinTotalBytes`, ten `Cubin*` `Stats` fields, bind in `Attach`, close in `Close` |
| `shim/Makefile` | `core/cubin.cc` into `CORE_SRC`, `cubin_test` into `test` |

**`gpuprobe/enroll.go` and `shim/core/enroll.{h,cc}` are byte-for-byte unchanged.**
The enrolment path is validated on hardware at 505/505 stacks and any edit there
re-opens #49. What the cubin channel reuses, it reuses by *calling*:

- `enrollShimIdentity` — the btrfs `stat`-vs-`maps` fix, inherited rather than
  re-derived;
- `enrollPeerCreds` and `procMapsHaveInode` — verbatim, so an offer is
  authenticated exactly as an enrolment is;
- `enrollRequiredUID` — the same unprivileged-consumer pinning;
- the `enrollAdmission` *type*, instantiated through `newCubinAdmission` as a
  **separate object with its own numbers**.

On the C++ side `cubin_name_from_maps` calls `enroll_name_from_maps` and
re-prefixes its result, so there is exactly one `/proc/<pid>/maps` parser and
exactly one dev:inode derivation feeding both channels. They cannot come to
disagree about which shim image they mean.

## Why a separate channel, restated as code

`shim/core/enroll.h` states the protocol as "The producer sends nothing" and
`enrollListener.handle` implements exactly that — creds, admit, uid/pid checks,
`procMapsHaveInode`, `reg.enroll`, one status byte, **never a read**. A cubin
offer must send a header. Sharing the socket would force a discriminating read,
and on a genuine enrolment that read blocks until the producer's 2 s budget
expires: every rendezvous becomes a 2 s stall ending in `kEnrollError`.

Three more hazards, all removed structurally rather than defensively:

| hazard on a shared socket | why it cannot happen here |
| --- | --- |
| `serve()` is serial **at `Accept`**, so offers queue *ahead* of a producer already in the backlog | the cubin listener has its own `net.UnixListener` and its own goroutine |
| admission charged per connection against `enrollUIDBurst = 32`, so a module-heavy workload has its own *enrolments* refused | `newCubinAdmission` is a separate bucket; `cubinUIDBurst = 128`, `cubinTotalBurst = 256` |
| a `connect()` and a 2 MB write on the application's `cuModuleLoad` path | the payload is a memfd passed by `SCM_RIGHTS`; nothing streams, and Task 5 puts the offer on the drain thread |

## The wire format

Second abstract address, same shape as `enrollAddressFor` (`enroll.go:246`),
different prefix:

```
@perfagent-gpu-cubin.v1.<dev_major>.<dev_minor>.<inode>
```

One `sendmsg`: a 24-byte header plus one descriptor in `SCM_RIGHTS`. Fixed
size, little-endian, naturally aligned — the same rules every USDT record
follows.

```
offset  size  field     value
     0     4  magic     'C' 'U' 'B' '1'   (0x31425543 read little-endian)
     4     2  version   1
     6     2  flags     0 — reserved; a non-zero one is REJECTED, never guessed
     8     8  size      declared cubin length in bytes
    16     8  crc       cuptiGetCubinCrc() over exactly those bytes
```

Reply: one byte, `'K'` accepted / `'X'` refused — the same spelling the
enrolment reply uses.

`crc` is the producer's **key**, not a checksum this end recomputes: CUPTI's
polynomial is unpublished and the agent has no CUDA toolkit. It is what
`gpu_pc_sample_batch_v1` joins on, so what matters is that the same number
reaches both ends — a hardware check, listed below.

The header is pinned from both sides. `shim/core/cubin.h` carries
`static_assert`s on `sizeof` and every `offsetof`;
`TestTheOfferHeaderCodecRoundTrips` and `test_the_header_is_the_documented_24_bytes`
read the same 24 bytes as bytes rather than as a struct, so a layout or
endianness drift cannot pass unnoticed.

## The seal verification

Producer (`cubin_seal_bytes`): `memfd_create(MFD_CLOEXEC|MFD_ALLOW_SEALING)`,
`write()` the bytes (never an mmap — `F_SEAL_WRITE` returns `EBUSY` against an
outstanding writable mapping), then `F_ADD_SEALS` with all four, then
`F_GET_SEALS` to confirm the kernel took them. A descriptor that reaches the
consumer unsealed would be silently unresolvable; catching it here makes it a
counted send failure with a reason instead.

Consumer (`verifyCubinSeals`), **before anything is mapped and before the fd is
even `fstat`ed**:

```go
seals, err := unix.FcntlInt(uintptr(fd), unix.F_GET_SEALS, 0)
if missing := cubinRequiredSeals &^ seals; missing != 0 { reject }
```

`cubinRequiredSeals = F_SEAL_SEAL | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_WRITE`.
All four, all-or-nothing:

- **`F_SEAL_SHRINK`** — without it a peer can `ftruncate` under our `mmap` and
  the next touched page **SIGBUSes the agent**. A profiler a profiled process
  can kill is worse than no profiler.
- **`F_SEAL_WRITE`** — without it the ELF mutates under the parser mid-parse.
- **`F_SEAL_GROW`** — without it the size we validated is not the size we map.
- **`F_SEAL_SEAL`** — without it the other three can be removed again, which
  makes checking them meaningless.

**There is no fallback branch that reads an unsealed offer anyway.** Falling
back is how a defended path becomes an undefended one. `F_GET_SEALS` also fails
with `EINVAL` on anything unsealable, so the same check refuses a pipe or a
socket; a tmpfs file — which *is* shmem, and so *is* sealable, answering with
`F_SEAL_SEAL` alone and no write protection — is refused for the three seals it
lacks. Both cases are tested, because the second is the dangerous one: it looks
like a sealed object and the peer can still write through it.

The mapping itself is `PROT_READ | MAP_PRIVATE`, one copy out, then `munmap`.

## Order of checks in `handle`

Cheapest and most hostile first; nothing touches the payload until the peer has
earned it, and within the payload the seals come before the size.

1. `enrollPeerCreds` → `unauthorized`
2. cubin admission bucket → `throttled` (so an exhausted peer cannot spend our `/proc` time either)
3. `requireUID` → `unauthorized`
4. per-PID filter → `unauthorized`
5. `procMapsHaveInode` → `unauthorized`
6. header + descriptor, under a 2 s deadline → `malformed`
7. **`F_GET_SEALS`** → `unsealed`
8. already-held CRC → `duplicate`, reply `'K'`, nothing mapped
9. per-cubin ceiling → `tooLarge`
10. total-bytes ceiling → `tooLarge`
11. `fstat` size vs declared size → `malformed`
12. `mmap`, copy, `munmap`, store → `received` + `bytes`

## The counters, and what each reads on a healthy run

The plan's nine, all implemented and all assertable. `assertNoCubinRejections`
checks the five rejection counters **at zero** on every healthy path in the
suite — ten defects on this project were counters reading green exactly when
things were worst, so the zero direction is asserted as hard as the non-zero one.

| counter | side | `Stats` field | healthy run | asserted non-zero by |
| --- | --- | --- | --- | --- |
| `CubinsOffered` | shim | `perfagent::cubins_offered()` | = modules handed over | `test_a_refusal_is_counted_as_a_send_failure` |
| `CubinsSendFailed` | shim | `perfagent::cubins_send_failed()` | **0** | same; `test_no_listener_is_not_a_send_failure` pins that an unprofiled process does **not** accumulate these |
| `CubinsReceived` | agent | `Stats.CubinsReceived` | = distinct modules stored | byte-identical test |
| `CubinBytesReceived` | agent | `Stats.CubinBytesReceived` | = their total size | byte-identical, total-ceiling tests |
| `CubinsRejectedTooLarge` | agent | `Stats.CubinsRejectedTooLarge` | **0** | per-cubin ceiling +1 byte; total ceiling |
| `CubinsRejectedMalformed` | agent | `Stats.CubinsRejectedMalformed` | **0** | 7 cases: magic, version, flag, zero size, short header, 0 fds, 2 fds; plus size mismatch and store failure |
| `CubinsRejectedUnsealed` | agent | `Stats.CubinsRejectedUnsealed` | **0** | each seal missing in turn (4), pipe, unsealed tmpfs file |
| `CubinsRejectedUnauthorized` | agent | `Stats.CubinsRejectedUnauthorized` | **0** | peer not mapping the shim; wrong PID under a per-PID attach |
| `CubinsThrottled` | agent | `Stats.CubinsThrottled` | **0** | drained-bucket test; both isolation orders |

**One counter beyond the nine.** The plan requires "an offer for an already-stored
CRC is a counted no-op", and none of the nine can hold that number without
lying: it is neither *stored* nor *rejected*. So `CubinsDuplicate` exists. It is
the tenth field and it is documented as such rather than folded into
`CubinsReceived`, which would overstate what the agent holds.

**Where the total-bytes ceiling is counted.** In `CubinsRejectedTooLarge`,
alongside the per-cubin ceiling, with the reason string distinguishing them
(`"per-cubin ceiling"` / `"total ceiling"`). This is a deliberate reading of the
plan, which names nine counters and puts both ceilings in the sentence beside
`CubinsRejectedTooLarge`. From the operator's side the two mean the same
actionable thing — *this cubin was refused for its size and nothing partial was
kept* — and both are tested separately. Flagged here so it is a choice on the
record and not an omission.

Two further `Stats` fields, matching what the enrolment listener already
surfaces: `CubinsListening` and `CubinsAddress`. The address is printed for the
reason `UnwindEnrollAddress` is: when the two ends derive different names every
counter on both sides reads zero and nothing says why.

`cubinStats.mapped` is an internal test seam, not a `Stats` field. It is the
only way a test can assert that a rejected offer was **never mapped**, which for
the seal check is the entire property.

## Bounds

| bound | default | config | counted |
| --- | --- | --- | --- |
| per cubin | 8 MiB | `Config.CubinMaxBytes` | `CubinsRejectedTooLarge` |
| total held | 256 MiB | `Config.CubinTotalBytes` | `CubinsRejectedTooLarge` |
| distinct CRCs | 512 | placeholder store, replaced by Task 4's LRU `gpu.ModuleStore` | store error → `CubinsRejectedMalformed` |

**An oversized cubin is rejected whole, never truncated.** A truncated cubin
parses into a *wrong* line table, which is the one failure worse than no line
table. `TestAnOfferOverThePerCubinCeilingIsRejectedWholeAndCounted` asserts
`received == 0`, `bytes == 0`, `mapped == 0` and `sink.putCount() == 0` — the
payload is not even mapped, so there is nothing a partial write could come from
— and then offers exactly the ceiling to show the rejection is one byte of
policy rather than an off-by-one.

## The isolation test — both orders, with output

`TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment`. Flood size
`cubinUIDBurst*2 + cubinTotalBurst` = 512 well-formed offers, well past both
buckets with margin for a second's refill. Well-formed on purpose: a flood of
connections that send nothing would exercise the header timeout rather than the
admission bucket and prove nothing about throttling.

**AHEAD** — the direction a shared socket fails. One genuine offer is wedged
inside the store so the cubin listener's serial `Accept` loop is occupied; 512
offers then queue in its backlog; *only then* does the enrolment dial. On a
shared socket this is precisely where the producer waits out its 2 s budget and
comes back `kEnrollError`.

**BEHIND** — the easy direction, included so the pair is symmetric rather than
selective: enrol, flood to `CubinsThrottled > 0`, enrol again.

```
=== RUN   TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment
=== RUN   TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment/offers_queued_AHEAD_of_the_enrolment
    cubin_test.go:808: cubin: offered=513 received=130 duplicate=0 throttled=383 | enrol: requests=1 confirmed=1 throttled=0
=== RUN   TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment/offers_queued_BEHIND_the_enrolment
    cubin_test.go:836: cubin: offered=512 received=129 duplicate=0 throttled=383 | enrol: requests=2 confirmed=2 throttled=0
--- PASS: TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment (0.06s)
    --- PASS: .../offers_queued_AHEAD_of_the_enrolment (0.05s)
    --- PASS: .../offers_queued_BEHIND_the_enrolment (0.01s)
```

383 cubin offers refused; `UnwindEnrollThrottled` **0** in both orders; both
enrolments confirmed, the AHEAD one inside 5 s while the cubin channel was
wedged with a full backlog. The property holds because the buckets and the
`Accept` loops are different objects — the test states it, the separation makes
it true.

## The regression that keeps them apart

`TestAnEnrolmentCompletesWithNoReadOnThatConnection`, asserted twice because
either half alone is escapable:

1. **Behaviourally** — a producer that writes nothing is still answered `'K'`,
   promptly.
2. **Structurally** — the test parses `gpuprobe/enroll.go` with `go/ast`, finds
   `enrollListener.handle`, and fails if its body calls anything that reads from
   the connection (`Read`, `ReadMsgUnix`, `ReadFull`, `NewScanner`, `Recvmsg`,
   `SetReadDeadline`, …). A behavioural test alone would pass for a handler that
   reads with a *short* deadline and falls through — which would still cost
   every producer that deadline, and would still be the shared-socket design
   creeping back one commit at a time.

## The cross-language seam

`TestTheCppProducerAndTheGoCubinListenerAgreeOnTheWire` builds the real producer
out of `shim/core/{cubin,enroll}.cc`, points the real Go listener at that
binary, and runs it — **on both filesystems**. The address embeds a device
number, and `stat(2)` and `/proc/<pid>/maps` report the *same* device on tmpfs
and *different* devices on btrfs, where the shim is actually built. A test that
ran only under `TMPDIR` would prove the two ends agree on the one filesystem
where they cannot disagree, which is exactly how the enrolment address once
shipped broken. The 5 KB fixture is generated independently in C++ and in Go, so
"byte-identical" is an assertion and not a tautology.

## Verification run

```
make -C shim && make -C shim test        cubin_test: OK, all 8 suites green
go build ./... && go vet ./...           clean
go test ./gpuprobe/ ./gpu/ ./internal/... -count=1
                                         ok gpuprobe 5.3s, gpu 3.5s, 9 internal pkgs
go test ./gpuprobe/ -race -count=4       ok 15.9s
~/go/bin/golangci-lint run --timeout=5m  0 issues
```

The cubin suite, 22 tests, 37 leaf cases, all passing:

```
--- PASS: TestTheCubinAddressIsASiblingOfTheRendezvousAndNotTheSameSocket
--- PASS: TestACubinArrivesByteIdenticalAtFiveKilobytesAndAtTwoMegabytes (5_KB, 2_MB)
--- PASS: TestAnOfferOverThePerCubinCeilingIsRejectedWholeAndCounted
--- PASS: TestTheTotalBytesCeilingIsEnforcedAndCounted
--- PASS: TestADeclaredSizeThatDisagreesWithThePayloadIsRejected (larger, smaller)
--- PASS: TestEachRequiredSealMissingInTurnIsRejectedAndNeverMapped
             (F_SEAL_SHRINK, F_SEAL_WRITE, F_SEAL_GROW, F_SEAL_SEAL, all_four_present)
--- PASS: TestADescriptorThatIsNotASealedMemfdIsRefused (pipe, unsealed tmpfs file)
--- PASS: TestAMalformedOfferIsRefusedAndCounted
             (bad magic, unknown version, reserved flag, zero size, short header,
              no descriptor, two descriptors)
--- PASS: TestACubinPeerThatDoesNotMapTheShimIsRefused
--- PASS: TestAPerPIDConsumerRefusesCubinsFromEveryOtherPID
--- PASS: TestAnOfferForAnAlreadyStoredCRCIsACountedNoOp
--- PASS: TestAStoreFailureIsCountedAndTheOfferIsRefused
--- PASS: TestTheCubinAdmissionBucketIsItsOwn
--- PASS: TestAThrottledCubinOfferIsReleasedAndCountedNotServed
--- PASS: TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment (AHEAD, BEHIND)
--- PASS: TestAnEnrolmentCompletesWithNoReadOnThatConnection
--- PASS: TestTheOfferHeaderCodecRoundTrips
--- PASS: TestASecondCubinListenerForTheSameShimIsRefused
--- PASS: TestClosingTheCubinListenerIsSafeAndReleasesPeers
--- PASS: TestTheBuiltInCubinStoreIsBounded
--- PASS: TestSealNamesAreSpeltOut
--- PASS: TestTheCppProducerAndTheGoCubinListenerAgreeOnTheWire (source fs, TMPDIR)
```

## Cannot verify — outstanding on the RTX 3090

The implementer has `CapEff: 0` and no GPU. Two claims in this task are
therefore **unverified**, exactly as the plan predicted, and neither is implied
by anything above:

1. **That a real `MODULE_LOADED` cubin survives byte-identical.** Everything
   here moves a synthetic fixture. The transport is proven byte-exact at 5 KB
   and 2 MB across the C++/Go boundary, but no cubin CUPTI actually produced has
   been through it. What could still differ: a real cubin's size distribution,
   and whether the adapter's copy (Task 5) is taken at a moment when CUPTI's
   buffer still holds the right bytes — §6.3 finding 2 measured that a late
   reader gets silently *wrong* bytes rather than a fault.

2. **That `cuptiGetCubinCrc()` over the received bytes equals the `cubinCrc` the
   PC records carry.** The agent does not recompute the CRC — it cannot, without
   a CUDA toolkit — so it trusts the header's value as the join key. If the two
   numbers disagree on hardware, every Tier B PC sample resolves to `no-module`
   with every counter reading green. This is the single most consequential thing
   the first GPU run must check.

Also untested here, and by design: the shim-side offer is a minimal client
(`cubin_offer` / `cubin_offer_to_consumer`) with no adapter wiring. The bounded
queue, the `MODULE_LOADED` copy-in-the-callback and the drain-thread send are
**Task 5**; `cubin.h` states the "never from a CUPTI callback" rule so Task 5
inherits it in writing rather than by recollection.
