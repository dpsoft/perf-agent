# Unresolved frames name their module

Branch: `feat/unresolved-frame-modules`. One commit. Not pushed, no PR.

Goal: a stack frame that was unwound correctly but could not be given a
symbol should read `libcuda.so.1+0x1b71c6`, not `0x7f2c945b2c2b`.

---

## Where the module was being lost, and why

Two independent losses, both real, both required for the fix. Neither is
the guess in the brief ("the pprof builder's unconditional default
mapping") on its own — that default is a *symptom* of the second.

### 1. blazesym returns nothing at all for an address it cannot name

This is the answer to the question the brief asked to settle first:
**no, blazesym does not populate `Frame.Module` when it fails.**

`blazesym/src/symbolize/symbolizer.rs` produces `Symbolized::Unknown(reason)`
for a miss — a variant that carries a `Reason` and *nothing else*. The
mapping was known one stack frame earlier (`handle_entry_addr` has the
`MapsEntry` in hand, path and all) and is discarded on the way out. The C
API then does this, `blazesym/capi/src/symbolize.rs:1105`:

```rust
Symbolized::Unknown(reason) => {
    // Unknown symbols/addresses are just represented with all
    // fields set to zero (except for reason).
    let () = unsafe { syms_last.write_bytes(0, 1) };
    sym_ref.reason = reason.into();
}
```

So `blaze_sym.module` is NULL and `offset` is 0 for exactly the frames that
need them. `symbolize/local.go`'s `fromBlazesymSym` faithfully copies that
nothing into `Frame.Module`, writes the hex PC into `Name` so the location
renders as *something*, and sets `Reason = FailureMissingSymbols`.

The module was never lost downstream. It never arrived.

### 2. The GPU pipeline builds its profile with no `procmap.Resolver`

`pprof.ProfileBuilder.addLocation` has only one way to reach a real
`profile.Mapping`: `p.resolver.Lookup(pid, addr)`. `cmd/gpu-cuda-profile`
and `cmd/gpu-stub-profile` construct their builders as
`pprof.BuildersOptions{SampleRate: 1}` — no `Resolver`. Every frame therefore
fell through to `addLocationByFallback` on `p.Profile.Mapping[0]`, the
unconditional `{ID: 1}` default.

`go tool pprof -raw /home/diego/gpu-cuda-45.pb.gz` confirms it exactly:

```
Locations
     9: 0x0 M=1 cudaLaunchKernel :0:0 s=0()
    10: 0x0 M=1 0x7f2c958b71c6 :0:0 s=0()
    ...
    16: 0x0 M=1 0x7f2c944de06b :0:0 s=0()
Mappings
1: 0x0/0x0/0x0
```

Note `0x0 M=1` on **every** location, including the resolved ones — the
address column is zero because the fallback path never sets `Location.Address`.
So the GPU path had no mapping for any frame, resolved or not.

### Why wiring a Resolver into the GPU builder is not the fix

It cannot work. The GPU tools build the profile *after* `cmd.Wait()` — the
workload has exited and `/proc/<pid>/maps` is gone. A build-time lookup finds
nothing. The mapping has to be captured **while the target is alive**, which
is the symbolization moment, not the build moment.

That settles the layering: **the fix is in the symbolizer**, and the frame
carries the mapping forward to the builder.

---

## What was built

```
blazesym miss  ->  symbolize.attachModules   (NEW, needs a live process)
                     fills Module/BuildID/MapStart/MapLimit/MapOff
               ->  symbolize.ToProfFrames    (carries them + Unresolved bit)
               ->  pprof.addLocation branch 3b (NEW: frame-carried mapping)
               ->  pprof.addLocationByAddr   (renames, only if Unresolved)
```

- **`internal/framename`** (new) owns the one textual form,
  `Format(modulePath, off)` and `IsAddressOnly(name)`. Three packages have to
  agree on it (pprof writes it, foldedstacks counts it, flamegraph colours it);
  a private copy in each is how the honesty counters drift apart.
- **`symbolize.Frame`** gains `MapStart/MapLimit/MapOff` and a
  `ModuleOffset() (uint64, bool)` accessor.
- **`symbolize/module.go`** (new): `ModuleIndex` interface (`*procmap.Resolver`
  satisfies it) and `attachModules`, which runs **only** over frames with
  `Reason != FailureNone && Module == ""`. A fully symbolized stack does zero
  lookups.
- **`symbolize.NewLocalSymbolizer`** takes options; `WithModuleIndex(idx)`
  supplies the index. Without one, nothing changes at all.
- **`pprof.Frame`** gains `Unresolved bool`, set by `ToProfFrames` from
  `Frame.Reason`. It has to be carried: after the conversion, `0x4017c2` is
  indistinguishable from a function genuinely called `0x4017c2`, and pprof has
  no unsymbolized bit.
- Wired at: `cmd/gpu-cuda-profile`, `cmd/gpu-stub-profile`,
  `perfagent/agent.go` (`chooseSymbolizer`), and the debuginfod symbolizer's
  local fallback.
- **`gpuprobe.Stats.StackFramesModuleOnly`** (new counter): the subset of
  `StackFramesUnresolved` that recovered a module. Zero-while-the-total-is-large
  is the "no index wired / target exited first" failure, and it is now visible
  instead of hidden inside the total.

---

## What an unresolved frame renders as

### pprof

`Function.Name` becomes `libcuda.so.550.54.14+0x14b71c6`, and `Location.Address`
is the same number, and `Location.Mapping` is a real mapping with the file,
start, limit, offset and build-id.

**Why `Function.Name` and not the flame graph's presentation layer:** the same
`.pb.gz` is read by `go tool pprof`, which renders `Function.Name` and knows
nothing about perf-agent's conventions. A name only the flame graph understood
would help half the audience. Putting it in the name serves both, and the real
`Mapping` serves a third audience — anything that wants to re-symbolize the
profile later.

**The offset is module-relative, not absolute.** It is `Address - MapStart +
MapOff`, i.e. the offset into the backing file — the same number pprof already
stores in `Location.Address`, stable across runs, and directly usable with
`addr2line -e libcuda.so.550.54.14 0x14b71c6`.

`Mapping.HasFunctions` is deliberately **not** set by an unresolved frame. The
mapping is in the profile because its symbols are missing; claiming otherwise
would mislead every downstream reader.

### Flame graph

The label is the same string. The colour needed a change, and this is the one
non-obvious edit in the diff:

`flamegraph.Classify` checked module-derived rules *before* the
`isUnsymbolized(name)` rule. That order was safe only because GPU profiles had
no modules. Once frames carry `libcuda.so.1`, an unnamed frame matches
`isVendorModule` and would have been painted as ordinary vendor code —
silently destroying `DomainUnsymbolized`, whose label is literally
*"vendor, no symbols"* and whose hatch is the only thing on the page saying no
symbol table named this address. The domain would have become unreachable for
vendor libraries, i.e. the graph would look cleanest exactly when
symbolization worked worst.

So `isUnsymbolized` now moves **above** the module rules (and recognizes the
new form), with `module == "[kernel]"` still above it so a raw kernel address
stays orange. Both grades of unresolved share the domain and are told apart by
their label — `libcuda.so.1+0x1b71c6` (module known) vs `0x7f2c945b2c2b`
(nothing known).

`foldedstacks.isAddressOnly` likewise recognizes the new form, so
`Result.AddressOnlyFrames` and the "N of M frame slots have no symbol" warning
report the identical gap before and after. A test asserts that equality
directly.

---

## The no-mapping case

A PC that no file-backed executable mapping covers stays a bare `0x...`, on
the default mapping, and is never merged with the has-module case. Four
independent guards:

1. `attachModules` only writes fields on a `Lookup` hit with a non-empty path;
   `procmap`'s parser already drops anonymous, non-executable and bracketed
   pseudo-file lines, so a hit is always a real file.
2. `pprof.frameMapping` additionally requires `MapStart <= Address < MapLimit`.
   A frame whose carried range does not contain its own address is refused
   outright rather than interned at a nonsense offset under a confident file name.
3. `framename.Format` returns `""` for `""`, `[kernel]` and `[jit]`, so the
   sentinel mappings can never become a module name.
4. It is *counted*, not just rendered: `LocalStats.ModulesAttached` /
   `ModulesBare`, and `gpuprobe.Stats.StackFramesModuleOnly` against
   `StackFramesUnresolved`.

One thing this cannot rule out, stated rather than hidden: a symbol whose whole
demangled name is an identifier, `+0x`, and hex digits — no parenthesis, colon,
space or bracket anywhere — is indistinguishable from the module form once the
profile is written, because the `Unresolved` bit does not survive into the
file. None has been observed. The consequence would be a frame drawn as
unsymbolized and counted in the gap warning: over-reporting the gap, never
hiding it. `TestKnownAmbiguity` pins it so nobody reads the strictness tests as
a proof it is impossible.

---

## What changes for the CPU profilers (`--profile`, `--offcpu`)

They share `pprof`, `symbolize` and the flame graph, so:

1. **Unresolved frames gain module names.** `perfagent/agent.go` now passes its
   `procmap.Resolver` to `NewLocalSymbolizer` as a module index, so a hex frame
   in a stripped library reads `libssl.so.3+0x4a120`. This is the intended
   benefit, arriving for free.
2. **`Mapping.HasFunctions` is no longer set by an unresolved frame.** Behaviour
   change. Previously any non-empty `Name` — including a hex address — set it.
   A mapping with at least one resolved frame still sets it. `TestMappingFlags`
   (which uses a named frame) passes unchanged.
3. **Flame graph colouring** of a hex-named frame with a known vendor/system
   module moves from yellow/red to the hatched unsymbolized domain (see above).
   A hex-named *kernel* frame is unaffected.
4. **No change to resolved frames**, anywhere. Branch 3 (the builder's own
   Resolver) still runs first and still wins, so existing mapping attribution
   is untouched; the new branch 3b only fires where branch 3 found nothing.
   `TestResolvedFrameIsNeverRenamed` and the byte-for-byte comparison in
   `TestGPUStack_After` pin this.
5. **No new hot-path cost.** The index is consulted per *failed* frame only.

Not changed, and worth knowing: neither `profile/` nor `offcpu/` ever calls
`Resolver.Invalidate`, so a PID that exits and is reused within one run keeps
the first process's mappings. That window is pre-existing — the builders
already attribute every user frame through the same cache — and this change
neither widens nor narrows it. Fixing it is separate work.

---

## Verification

```
go build ./... && go vet ./... && go test ./... -count=1   # all pass
~/go/bin/golangci-lint run --timeout=5m                    # 0 issues
cd test && go vet ./...                                    # separate module, clean
```

New tests:

- `internal/framename/framename_test.go` — format, recognition, the
  round-trip property that everything `Format` emits `IsAddressOnly` accepts,
  and the acknowledged ambiguity.
- `symbolize/module_test.go` — module attached; **resolved frames not touched
  and not even looked up**; a module the symbolizer already knew is not
  overwritten; unmapped address stays bare with all fields zero; nil index
  counted as bare; `ModuleOffset` rejects an address outside its own mapping;
  `ToProfFrames` carries the fields and the `Unresolved` bit, and never marks
  an inlined frame.
- `pprof/unresolved_test.go` — the rename, the mapping, name/`Location.Address`
  agreement, `HasFunctions` false, no-mapping stays bare on mapping 1,
  inconsistent carried range refused, resolved frame never renamed, `[kernel]`
  never used as a module name, Resolver still wins, two offsets in one module
  stay two frames.
- `internal/foldedstacks/gpu_stack_e2e_test.go` — the whole pipeline over the
  real stack (below).
- `internal/flamegraph/domain_test.go` — unsymbolized beats module; kernel
  still beats unsymbolized.

### What the new pipeline produces for the real 16-frame stack

The stack is transcribed from `go tool pprof -raw` on
`/home/diego/gpu-cuda-45.pb.gz` (locations 4..18 then 2). `TestGPUStack_Before`
reproduces the file's current output; `TestGPUStack_After` shows the new one:

```
_start                                                      _start
__libc_start_main_alias_1                                   __libc_start_main_alias_1
__libc_start_call_main                                      __libc_start_call_main
main                                                        main
__device_stub__Z14perfagent_axpy...                         __device_stub__Z14perfagent_axpy...
cudaLaunchKernel                                            cudaLaunchKernel
0x7f2c958b71c6                          ->                  libcuda.so.550.54.14+0x14b71c6
0x7f2c945ace62                          ->                  libcuda.so.550.54.14+0x1ace62
0x7f2c945acc75                          ->                  libcuda.so.550.54.14+0x1acc75
0x7f2c945b2dfb                          ->                  libcuda.so.550.54.14+0x1b2dfb
0x7f2c945b2c2b                          ->                  libcuda.so.550.54.14+0x1b2c2b
0x7f2c945bbf6f                          ->                  libcuda.so.550.54.14+0x1bbf6f
0x7f2c944de06b                          ->                  libcuda.so.550.54.14+0xde06b
(anonymous namespace)::on_callback(...)                     (anonymous namespace)::on_callback(...)
[gpu:launch]                                                [gpu:launch]
[gpu:kernel:_Z14perfagent_axpyfPKfPfi]                      [gpu:kernel:_Z14perfagent_axpyfPKfPfi]

Mappings: 1: 0x0/0x0/0x0          ->      + libcuda.so.550.54.14 @ 0x7f2c94400000
```

The mapping *ranges* in that test are a labelled fixture: the real run's
`/proc/<pid>/maps` was never recorded, so the range is chosen to contain the
observed addresses. It is not a claim about where libcuda sat on that machine.
What is real is the stack, the frame names, the seven addresses, and the
arithmetic.

`TestGPUStack_SymbolizationGapStillReported` asserts `AddressOnlyFrames == 7`
in **both** columns.

### Cannot verify end to end

**This has not been confirmed against a live GPU.** This box has `CapEff: 0`
and no GPU; the existing capture cannot be re-rendered because the mapping data
was never written into it, which is the whole point of the bug. Everything
above is unit-level proof of the mechanism plus a reconstruction of the real
stack.

A real confirmation needs a fresh capture on the RTX 3090, which the controller
will run. What to look for:

1. `go tool pprof -raw <new>.pb.gz` shows **more than one** mapping, with
   libcuda's real path, start, limit and build-id, and locations reading
   `0x<offset> M=<n>` rather than `0x0 M=1`.
2. The seven frames render as `libcuda.so...+0x...` / `libcupti.so...+0x...`.
3. The run's stats line shows `StackFramesModuleOnly` close to
   `StackFramesUnresolved`. If it is 0 while the latter is large, the module
   index is not reaching the symbolizer, or the workload is exiting before its
   stacks are drained.
4. The flame graph's symbolization-gap warning still reports roughly 15%, not 0%.
