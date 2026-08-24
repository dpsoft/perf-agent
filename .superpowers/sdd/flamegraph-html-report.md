# `--flamegraph-output`: a self-contained HTML flame graph

Branch `feat/flamegraph-html`. One commit. Additive: two new packages, one new
`cmd/`, plus flag/option/Stop wiring. Nothing in `gpu/`, `gpuprobe/`, `bpf/`,
`unwind/` or `shim/` was touched.

---

## 1. What shipped

| Piece | Path |
|---|---|
| pprof → folded stacks | `internal/foldedstacks/fold.go` |
| Domain classifier | `internal/flamegraph/domain.go` |
| HTML/SVG renderer | `internal/flamegraph/render.go`, `assets.go` |
| Profile file → page | `internal/flamegraph/file.go` |
| Offline CLI | `cmd/flamegraph/main.go` |
| Agent wiring | `perfagent/options.go`, `perfagent/agent.go` |
| Flag | `main.go` (`--flamegraph-output`) |

`--flamegraph-output <path>|auto` sits beside `--profile-output` and
`--offcpu-output`. `auto` follows the established convention exactly —
`generateOutputName(pid, all, "on-cpu"|"off-cpu", "html")`, the same function
`--pmu-output auto` calls. With `--profile` and `--offcpu` both on, an explicit
path is rejected at flag-parse time: one path cannot name two graphs, and
rendering one while silently dropping the other is the failure mode this whole
feature is supposed to avoid.

The renderer reads the `.pb.gz` back off disk after the agent writes it. That is
deliberate: the page is a rendering of the artifact the user was actually
handed, so a page that looks wrong is evidence about the file rather than about
a second, parallel code path. The consequence is that `WithCPUProfileWriter`
(stream-to-io.Writer) has no file to re-read; that case logs why no graph
appeared rather than staying quiet.

---

## 2. What was salvaged from PR #10, and what changed

**Kept:** the overall shape — build a tree from folded stacks, emit one `<g
class="frame">` per node with `data-orig-x`/`data-orig-width`, rewrite `x`/`width`
in JS to zoom, no d3, no CDN, one file. The click-to-zoom ancestor/descendant
test, the `/`-to-search and Esc-clears-then-resets keybindings, and the
breadcrumb/details/match-count toolbar are PR #10's design and survive.

**Changed, with reasons:**

1. **It could not read a pprof file.** PR #10 consumed folded text and nothing
   in the repo produced it. That gap is why it never shipped; `internal/foldedstacks`
   is the missing half.

2. **The renderer no longer round-trips through text.** It consumes
   `[]foldedstacks.Stack` directly. Folded text has no escape for `;`, a space,
   or a newline inside a frame name, and C++ symbols contain spaces routinely
   (`foo(float, int)`). PR #10's `strings.LastIndexByte(line, ' ')` parser
   happens to survive that, but a `;` in a name would silently split one frame
   into two. `WriteFolded` still exists (and `cmd/flamegraph -folded` exposes it)
   but it *reports* how many names it had to substitute, and the structured path
   the renderer uses keeps the originals.

3. **An empty profile was an error.** `renderParsed` returned
   `fmt.Errorf("no folded samples")`. Now a degenerate profile renders a page
   that states the fact — see §5.

4. **"samples" was hardcoded.** PR #10's UI said `N samples` for every value.
   This repo's GPU and off-CPU profiles are nanoseconds; calling 5,987,854
   nanoseconds "5,987,854 samples" is a straightforward lie. The page now
   carries the profile's own `type/unit` and formats accordingly (ns/µs/ms/s),
   in Go for the tooltips and in mirrored JS for the live readouts.

5. **Colour was `fnv(name) % 360`.** Replaced with a domain classifier (§4).
   A hash carries no information, and these profiles have real structure.

6. **No synthetic root.** PR #10 laid out `root.children` side by side. Any
   profile with more than one root frame — every system-wide profile, and the
   verification profile itself, which has two disjoint roots — then had no
   full-width frame to click, and no anchor for "% of total". Added an `all`
   node.

7. **Zoom did not re-fit labels.** Text was truncated server-side for the
   original width and never recomputed, so a frame zoomed from 84px to 1264px
   still read `cudaLau…`. `fit()` now re-truncates on every layout.

8. **`fmt.Fprintf` returns were discarded** throughout (errcheck). Everything
   goes through an `errWriter` that latches the first error.

9. **One giant `Fprintf` format string** held the entire document, with
   `%%`-escaped CSS percentages and positional `%d`s interleaved with content.
   CSS and JS are now plain constants written with `io.WriteString`; no user
   data is ever a format argument or lands inside `<style>`/`<script>`.

10. **`RenderSVG` refuses a degenerate profile** rather than emitting a blank
    canvas, because the standalone SVG has nowhere to put the explanation.

11. Siblings are sorted by name and stacks are sorted before rendering, so two
    runs over one profile produce byte-identical HTML (tested). PR #10's output
    depended on input line order.

---

## 3. Stack order — the decision that was easiest to get silently wrong

The pprof proto specifies `Sample.Location[0]` is the **leaf**. perf-agent does
not follow that: `profile/profiler.go:236` and `offcpu/profiler.go:206` both
call `pprof.Reverse` before building, and `gpu/projection.go` assembles frames
root-first. In every profile this repo writes, `Location[0]` is the **root**.

Folding with the wrong assumption does not fail. It draws a perfectly plausible
flame graph upside down. So:

- `foldedstacks.Options.StackOrder` is explicit, defaulting to `RootFirst`
  (what perf-agent writes), with a `LeafFirst` mode for foreign profiles.
- `cmd/flamegraph -stack-order` exposes it.
- **The page always states which order it read**, in a header chip and in the
  footer. If it guessed wrong, the reader can see that it guessed.

---

## 4. Colour: domain, not hash

`Classify(name, module)` maps a frame to one of nine domains. Where the profile
supplies a mapping file it is used (`[kernel]`, `libcuda*`, `libperfagent*`,
`libc*`); otherwise the symbol name. The legend says the domain is *inferred*,
and classification affects nothing but colour — every width and position comes
from the measured value.

On the verification profile every frame lands correctly (test:
`TestClassifyCoversTheRealRTX3090Stack`):

| Frames | Domain | Treatment |
|---|---|---|
| `_start`, `__libc_start_main_alias_1`, `__libc_start_call_main` | process startup / libc | red |
| `main`, `__device_stub__Z14perfagent_axpy…` | application | pink |
| `cudaLaunchKernel` | GPU runtime, CPU side | orange |
| `0x7f2c958b71c6` … ×7 | unsymbolized | muted slate **+ hatch** |
| `(anonymous namespace)::on_callback(…CUpti_CallbackDomain…)` | perf-agent's own shim | violet |
| `[gpu:launch]` | CPU→GPU boundary | grey, outlined |
| `[gpu:launch unsampled]` | GPU work with no CPU stack | pale grey **+ hatch** |
| `[gpu:kernel:…]` | GPU kernel execution | green |

Two of these are honesty features rather than decoration:

- **Unsymbolized** frames are hatched because the depth is real and the *names*
  are missing — a symbolization gap, not an unwinding one. 1,799 of 11,598 frame
  slots in the verification profile.
- **`[gpu:launch unsampled]`** must not be confusable with `[gpu:launch]`. One
  means "GPU time we have a CPU stack for", the other "GPU time we do not". They
  differ in fill, in stroke, and in hatching, and the legend spells out the
  difference. A test asserts they cannot render identically.

The shim domain requires *both* `on_callback` and a vendor callback type in the
signature, so an application function innocently named `on_callback` is not
billed to the profiler.

The legend lists only domains actually present, so a CPU-only profile does not
advertise a GPU layer it does not have.

---

## 5. Degenerate profiles

`Fold` returns an empty profile as a `Result` with `Total == 0` and a populated
`Warnings`, never as an error and never as a silent success. `RenderHTML` then
emits **no `<svg>` and no `<script>`** and instead a panel:

> **No flame graph was drawn.** The profile contains zero samples. An empty
> flame graph would be indistinguishable from a flame graph of an idle process,
> so none is drawn. […] This is the profile reporting on itself, not a rendering
> failure.

The all-zero-values case gets its own wording. The same warnings are echoed to
the terminal by both `cmd/flamegraph` and the agent, so a pipeline sees it
without opening a browser.

Verified against a real file: `/tmp/fg/empty.pb.gz`, a valid gzipped pprof with
a declared `cpu/nanoseconds` type and zero samples.

```
$ go run ./cmd/flamegraph -o /tmp/fg/empty.html /tmp/fg/empty.pb.gz
wrote /tmp/fg/empty.html
  axis     cpu/nanoseconds
  total    0 ns across 0 samples in 0 distinct stacks
  note     This profile contains no samples at all. Nothing was collected, so
           there is nothing to draw. That is a fact about the profile, not a
           statement that the target was idle.
```

The rendered page contains no `<svg class="flame">` and no `<script>` (asserted
in `TestFromProfileFileWritesAPageForAnEmptyProfile`).

---

## 6. Sample-type choice

`chooseSampleIndex`:

1. `Profile.DefaultSampleType`, when it names a real type;
2. otherwise the **last** sample type.

"Last" matches `google/pprof`'s own driver. Every mode perf-agent ships today —
`cpu`, `offcpu`, `gpu` — declares exactly one type, so this only bites the
memory builder, where the two types are `alloc_objects/count` and
`alloc_space/bytes` and "last" picks bytes, which is the useful axis. When a
profile has more than one axis the page says which one it drew and names the
ones it did not.

**Negative values are refused, not drawn.** A negative value means a
`-diff_base` differential profile. A flame graph has no negative rectangle, and
clamping or absolute-valuing one would present a subtraction as a measurement.

For the verification profile: one type, `gpu/nanoseconds`, index 0.

---

## 7. GPU labels — the decision, and its grounding in spec §8

**Decision: labels stay out of the frames entirely. They are summarised beside
the graph, and the two that qualify how much the graph can be trusted are
promoted to warnings on the page.**

### Why not in the frames

§8 of `docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` already
ruled on this, and the ruling is not ambiguous:

> - **Frames** — what should nest in the flame graph: real CPU stack, then
>   `[gpu:launch]`, then the kernel name.
> - **Labels** — what must not fragment aggregation: stall reason, PC, queue,
>   device, correlation ID, cgroup, pod UID, container ID.

and the reason:

> Frames are stack identity, so `[gpu:pc:0x1a40]` as a frame produces one
> flame-graph leaf per sampled instruction. […] this destroys aggregation.

The verification profile makes the cost concrete. It has 4,000 samples and
**4,000 distinct `gpu_correlation` values** — one per sample. Folding
`gpu_correlation` into the path turns 3 folded stacks into 4,000, each 1/4000th
of the width, and the flame graph stops being a flame graph. So the folder never
puts a label in a frame, and a test asserts it
(`TestFoldKeepsLabelsOutOfFramesAndSummarisesThemInstead`).

### Why not simply dropped

"Frames-only" cannot mean "labels vanish". The page therefore carries a
**Sample labels (not in the graph)** table: key, sample count, distinct-value
count, and the top values by value. For a label whose cardinality equals its
sample count it says *"one distinct value per sample — nothing to aggregate,
which is why this is a label and not a frame"* rather than listing eight
arbitrary correlation IDs that would read as a top-N finding.

### The two labels that change how the graph must be read

Two labels are not merely context; they qualify the trustworthiness of the very
frames being drawn. Leaving those to a table nobody scrolls to would be the
"looks healthy exactly when it is worst" defect in visual form.

**`gpu_join` / `gpu_ambiguous` → per-frame hatching.** `gpu/projection.go` sets
`gpu_join` unconditionally to `exact`, `heuristic` or `unmatched`, with an
explicit comment that "an ABSENT label must never be readable as 'exact'". A
heuristic join means the CPU call path under `[gpu:launch]` was *guessed*; the
tags it carries (pod_uid, container_id) may belong to the wrong container. So
`Fold` accumulates a per-stack `Inexact` weight from those labels, the renderer
hatches every affected frame with a distinct pattern, the tooltip says how much
of that frame is inferred, and a warning states the share of the total. On the
verification profile this is 0% — every join is `exact` — and correctly nothing
is hatched for it.

**`gpu_sample_period` → a warning about what the widths mean.** This is the one
that would have misled a reader worst. On the verification profile the picture
is:

- `[gpu:launch unsampled]` — 5,590,723 ns, **93.4%** of the total
- the joined 16-frame CPU stack — 397,131 ns, **6.6%**

The naive reading is "only 6.6% of GPU time comes from this call path". That is
wrong. `gpu/projection.go` samples launch *stacks* one-in-`SamplePeriod` (here
8) while recording *every* execution, and deliberately refuses to scale either
population, because "multiplying the sampled population by SamplePeriod would
turn a measurement into an estimate and present it as fact". So the 6.6% is a
sample of launches, not a share of GPU time. The page says so, in the notices
box, in these words:

> 93.4% of the total sits under [gpu:launch unsampled]: measured GPU time whose
> launch was not one of the sampled ones, so it has no CPU caller and none is
> borrowed from a sampled sibling.
>
> 257 samples carry gpu_sample_period=8: one launch in 8 contributed a CPU
> stack. The call path beneath [gpu:launch] is therefore a sample of launches,
> not a sample of GPU time - do not read its width as the share of GPU work that
> call path is responsible for. Nothing here is scaled by 8; every value is a
> measured duration.

Nothing on the page is scaled by the period. The flame graph does not imply
anything the profile does not say.

---

## 8. Verification against `/home/diego/gpu-cuda-45.pb.gz`

### Totals

| | `go tool pprof -raw` | `cmd/flamegraph` |
|---|---:|---:|
| total | 5,987,854 ns | 5,987,854 ns (`data-total="5987854"`, "5.99 ms") |
| samples | 4,000 | 4,000 |
| distinct stacks | 3 | 3 |
| max depth | 16 | 16 |

### Per-stack, exact

`go tool pprof -raw`, summed per distinct location list, against the folded
output — identical on all three:

| stack | pprof `-raw` | folded | samples |
|---|---:|---:|---:|
| `[gpu:launch unsampled];[gpu:kernel:_Z15perfagent_scalePffi]` | 2,894,901 | 2,894,901 | 2,000 |
| `[gpu:launch unsampled];[gpu:kernel:_Z14perfagent_axpyfPKfPfi]` | 2,695,822 | 2,695,822 | 1,743 |
| `_start;…;[gpu:launch];[gpu:kernel:_Z14perfagent_axpyfPKfPfi]` (16 frames) | 397,131 | 397,131 | 257 |
| **total** | **5,987,854** | **5,987,854** | **4,000** |

`go tool pprof -traces` reproduces the same three stacks with the same sample
counts (2,000 / 1,743 / 257). Its per-line values are printed rounded to three
significant figures (`1.44us`), so summing its display text gives 5,987,350 —
a 0.008% display artifact of `-traces`, not a discrepancy. `-raw` carries the
unrounded integers and matches exactly.

### Frame accounting

- Frame slots: 1,743×2 + 2,000×2 + 257×16 = **11,598** — matches.
- Unsymbolized: 257×7 hex frames = **1,799** (15.5%) — matches, and all seven
  are hatched in the render.
- Tree nodes: 1 root + 3 (unsampled branch) + 16 (joined chain) = **20** `g.frame`
  elements in the SVG — matches.

### The page itself

Rendered to `/tmp/fg/gpu-cuda-45.html` (30 KB) and opened in Chrome from
`file://`, light and dark, plus a headless driver that exercises the UI:

- **Self-contained.** Zero `<script src>`, `<link>`, `@import`, `<img>`,
  `fetch(`. The only `http://` in the whole document is
  `http://www.w3.org/2000/svg`, the SVG XML namespace — an identifier, not a
  fetch. Asserted by test, over the full URL set.
- **Zoom.** Clicking `_start` (83.8px) expands it to the full 1264px plot,
  hides the `[gpu:launch unsampled]` branch, sets focus to `_start`, breadcrumbs
  to `all › _start`, and details to
  `_start — 397.13 µs (6.63% of total) — process startup and libc`. The
  descendant `[gpu:kernel:…]` at depth 16 scales to 1264px; the same-named node
  at depth 2 in the other branch is hidden. `Reset zoom` restores 83.832px.
- **Search.** `gpu:kernel` reports `3 frames, 5.99 ms` (2,695,822 + 2,894,901 +
  397,131 = 5,987,854 ✓), marks the three matches, dims `_start`. `Esc` clears
  the box, resets the count, and un-dims.
- **Dark mode** renders correctly under `prefers-color-scheme: dark`.

### Escaping

Symbol names reach the page only as `html.EscapeString`-ed SVG attributes and
`<title>` text, and are read back with `dataset`/`textContent`. Tested with
`std::vector<Foo&Bar>::push_back("</title><script>alert(1)</script>")` and
`</g></svg><script>evil()</script>` as frame names and a script-injecting page
title: neither payload survives, and the document still contains exactly one
`<script>`, one `</script>` and one `<style>`. A separate test asserts no
profile-derived text ever appears inside `<style>` or `<script>`.

---

## 9. Cannot verify

- **No live capture.** This environment has no BPF capabilities, so
  `--flamegraph-output` was never exercised end to end through a real
  `agent.Start()` / `agent.Stop()`. What *was* exercised: flag parsing and both
  fatal validations against the built binary; `flamegraphPath` auto-naming
  against `generateOutputName`; and `Agent.writeFlamegraph` /
  `warnFlamegraphNeedsPath` directly, in `perfagent/flamegraph_test.go`, against
  real `.pb.gz` files on disk. The call sites in `Stop()` are three lines each
  and compile, but nothing has run them.
- **No AMD/ROCm profile.** The domain classifier's `hip*`, `hsa_*`,
  `rocprofiler*` and `libamdhip*` rules are written from the spec and from
  `shim/`, not observed against a real ROCm capture.
- **No kernel-stack profile.** The `[kernel]` mapping rule is unit-tested
  against a synthetic mapping; `--kernel-stacks` output was never folded.
- **No inlined-frame profile from this repo.** Multi-`Line` locations are
  unit-tested (`TestFoldExpandsInlinedFramesCallerFirst`, which pins the
  caller-first ordering the pprof proto specifies) but the verification profile
  has none, so the ordering has not been checked against a real DWARF-inlined
  stack.
- **No off-CPU or multi-axis profile rendered.** Both paths are unit-tested;
  neither has been run against a real file.
- **Firefox and Safari untested.** Verified in Chrome only. The page uses no
  APIs newer than `classList.toggle` and `dataset`, but that is reasoning, not
  evidence.
- **`isAddressOnly` is a name-shaped test.** Once a profile is written there is
  no "unsymbolized" bit left in pprof to consult — perf-agent's symbolizer has
  already turned the PC into the frame's *name*. A real symbol literally named
  `0xabc` would be miscounted. No such symbol is known to exist.
- **Domain classification is inferred, and says so.** Where the profile carries
  no mapping (which is every GPU profile today, since the GPU builder emits one
  empty mapping) the classifier reads symbol names. It could mislabel a frame.
  The legend states this; nothing about a frame's value, width or position
  depends on it.

---

## 10. Verification commands

```
$ go build ./... && go vet ./...            # clean
$ go test ./... -count=1                    # all packages ok
$ ~/go/bin/golangci-lint run --timeout=5m   # 0 issues
```

New tests: 23 in `internal/foldedstacks`, 30 in `internal/flamegraph`, 8 in
`perfagent`, 3 in `main` — 64 in total.
