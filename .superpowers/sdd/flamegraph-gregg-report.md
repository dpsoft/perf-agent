# Recolouring the flame graph to Brendan Gregg's AI Flame Graph palette

Branch `feat/flamegraph-gregg-palette`. One commit, touching
`internal/flamegraph/{domain.go,assets.go,render.go}` plus a new
`internal/flamegraph/palette_test.go`.

Nothing structural changed. The renderer still colours by domain, still
classifies from the profile's mapping first and the symbol name second, still
draws and counts unsymbolized frames, still refuses to draw a degenerate
profile, still emits one file with no network fetches. Only the palette, the
domain→colour mapping and the legend vocabulary moved.

## The mapping as built

Gregg's layers are pink (application), red/yellow/orange (the CPU code paths
that initiate GPU work: C, C++, kernel — in that order), grey (the boundary
between CPU and accelerator), aqua (source of functions running on the
accelerator), green (accelerator execution).

| domain (`data-domain`) | legend label | Gregg layer | light `hsl()` | rendered light | rendered dark |
|---|---|---|---|---|---|
| `app` | application | pink | `335 78% 79%` | `#f3a0c2` | `#e778a7` |
| `system` | CPU: process startup and libc | red (C) | `2 68% 78%` | `#eda3a1` | `#df7f7b` |
| `vendor` | CPU: GPU runtime and driver | yellow (C++) | `50 88% 69%` | `#f6de6a` | `#e9ce44` |
| `kernel` | CPU: kernel | orange (kernel) | `28 92% 70%` | `#f9ae6c` | `#ed9345` |
| `unsym` | vendor, no symbols | — (see below) | `45 20% 77%` + hatch | `#d0cab9` | `#bab29c` |
| `shim` | perf-agent | — (see below) | `215 15% 77%` + cool outline | `#bcc3cd` | `#9fa9b6` |
| `boundary` | CPU→GPU boundary | grey | `0 0% 84%` + dark outline | `#d6d6d6` | `#bdbdbd` |
| `boundary-unattributed` | GPU work with no CPU stack | grey | `0 0% 92%` + hatch + dashed outline | `#ebebeb` | `#d1d1d1` |
| `gpu-kernel` | GPU kernel execution | green | `128 48% 67%` | `#82d38d` | `#62c16f` |
| *(reserved)* | aqua | aqua | `184 52% 65%` | `#77ced4` | `#57bbc2` |
| `root` | all (synthetic, never in the legend) | — | `30 8% 81%` | `#d2cfcb` | `#bab5b0` |

The red/yellow/orange assignment follows Gregg's own convention rather than
being invented: C is red, C++ is yellow, the kernel is orange. That means the
previous colours for `kernel` and `vendor` swapped — `cudaLaunchKernel` is a
C++ runtime frame and is now yellow, kernel-mode frames are now orange.

`--fill-accel-source` (aqua) is declared and **no domain points at it**.
It belongs to Gregg's accelerator *source* layer, which needs GPU PC sampling
with SASS→source resolution; perf-agent does not emit those frames yet. The
legend on every page says so in one sentence with a swatch, so the hue is
visibly held open rather than silently missing. `TestAquaIsReservedAndUnused`
fails if a domain ever takes it without meaning to.

## The two domains Gregg has no layer for

Neither gets a sixth hue. Both are drained almost to grey, which is the
palette's own way of saying "this is not a layer of your computation", and
they lean in opposite directions so they stay tellable apart from each other
and from the pure-neutral boundary greys.

**Vendor unsymbolized** (`unsym`) keeps the warm CPU band's hue at 20%
saturation — a pale sand — and **keeps the hatch**. The hatch was carrying
real information (the depth is real, the names are missing) and it survives
unchanged; the recolour only gives the fill under it a reason. The reading is
"right layer, no name": warm enough to sit with the CPU frames it is always
sandwiched between, colourless enough never to be mistaken for one of the
named CPU layers, and never stealing the pure grey the boundary owns.

**perf-agent's own shim** (`shim`) gets the opposite drain: a cool slate at
15% saturation with a cool outline. The instrument sits visibly outside the
warm CPU band without claiming a hue. It was purple before, which was exactly
the sixth hue the brief asked to avoid.

The three near-greys are distinguished by temperature and outline, not by
lightness alone: warm + hatched + no outline (`unsym`), cool + solid cool
outline (`shim`), neutral + solid dark outline (`boundary`).

## Keeping the two boundaries apart

`[gpu:launch]` and `[gpu:launch unsampled]` mean different things — GPU time
we have a CPU stack for versus GPU time we do not — so sharing Gregg's grey
costs them a distinction the recolour must not lose. Three signals carry it:
the unattributed one is 8 lightness points paler, it is hatched, and it is the
only dashed outline on the page. `TestTheTwoBoundariesStayTellableApart`
asserts all three.

## Both themes, through the token system

Every fill is one `hsl()` written once, at `:root`, with its **hue fixed**.
The dark theme moves exactly two numbers:

```css
@media (prefers-color-scheme: dark){
  :root{ --fill-dl:-9%; --fill-ds:.9; --hatch-ink:rgb(22 20 18 / .20); }
}
```

`--fill-dl` shifts every fill's and every edge's lightness; `--fill-ds` scales
every fill's saturation. No colour is defined inside the media block, so the
two themes cannot drift into two designs.
`TestDarkThemeMovesOnlyTheThemeKnobs` fails if a third property ever appears
there, or if a second `@media` block does.

Because the fills stay light in both themes, the label ink, the hover outline
and the search-match outline do **not** follow the page theme —
`--frame-ink` has no dark variant. The previous code used `var(--ink)` for the
hover outline, which inverted with the page and went nearly invisible on a
light fill in dark mode; that is fixed.

Two mechanical consequences worth recording:

- Colour now reaches a frame through `data-domain` and a CSS rule, not through
  a `fill=` attribute. That is what makes it themeable. It also fixed a latent
  bug: the old `stroke=` presentation attribute on the boundary frames was
  being overridden by `g.frame rect.bg{stroke:…}` in the stylesheet, so the
  boundary outline never actually rendered in the HTML page.
- `RenderSVG` (standalone, for embedding) now emits the palette inside the
  `<svg>`, so a graph pulled out of the page keeps its colours *and* its
  theme response. `RenderHTML` does not duplicate it. Verified by rendering
  the real profile to a bare `.svg` and sampling pixels: `#82d38d` green,
  `#d6d6d6` boundary, `#bcc3cd` shim, `#d0cab9` unsym — the tokens exactly.

## Contrast check

Frame labels are 11px monospace in `--frame-ink` (`hsl(30 12% 9%)`) drawn on
the fills. Computed WCAG 2.1 contrast for **every** fill in **both** themes,
including the reserved aqua and the synthetic root. Two figures per fill: the
plain fill, and the fill with the hatch ink composited over it at *full*
coverage — the pessimistic reading, since the real stripes cover about a third
of a frame.

| fill | light plain | light hatched | dark plain | dark hatched |
|---|---|---|---|---|
| app | 9.02 | 5.41 | 6.75 | 4.71 |
| system | 8.83 | 5.31 | **6.57** | **4.60** |
| vendor | 13.27 | 7.64 | 11.48 | 7.66 |
| kernel | 9.60 | 5.71 | 7.74 | 5.33 |
| unsym | 10.93 | 6.42 | 8.70 | 5.95 |
| shim | 10.04 | 5.96 | 7.75 | 5.35 |
| boundary | 12.32 | 7.15 | 9.74 | 6.59 |
| boundary-unattributed | 14.93 | 8.51 | 12.01 | 7.99 |
| gpu-kernel | 9.94 | 5.89 | 8.11 | 5.56 |
| accel-source (aqua) | 9.84 | 5.84 | 8.04 | 5.52 |
| root | 11.48 | 6.71 | 9.04 | 6.16 |

Worst case anywhere: **4.60:1**, on hatched red in the dark theme. WCAG AA for
this text size is 4.5:1, so every pair passes with margin, including the two
the brief flagged as risky: pink bottoms out at 4.71 and aqua at 5.52.

This is not a one-off calculation. `TestLabelsStayLegibleOnEveryFillInBothThemes`
parses `paletteCSS` back into numbers — reading the stylesheet, not a
duplicate table in Go — applies the two theme knobs, and fails below 4.5:1.
Red was the binding constraint and was lightened from `2 72% 73%` to
`2 68% 78%` to clear it; the numbers above are the tuned palette. An
independent Python implementation of the same computation agreed with the Go
test to two decimal places.

## Verification performed

- `go build ./... && go vet ./... && go test ./... -count=1` — all packages
  pass. `~/go/bin/golangci-lint run --timeout=5m` — 0 issues.
- Rendered `/home/diego/gpu-cuda-45.pb.gz` (RTX 3090, 4000 samples, 3 distinct
  stacks, max depth 16). Total unchanged: `data-total="5987854"`, header chip
  reads 5.99 ms, and the synthetic root still carries the whole profile.
- **Looked at it**, in Chrome headless, light and dark. The 16-frame joined
  stack reads root-first exactly as intended: `_start`,
  `__libc_start_main_alias_1`, `__libc_start_call_main` red → `main`,
  `__device_stub__Z14perfagent_axpy…` pink → `cudaLaunchKernel` yellow → the
  seven `0x7f2c…` frames pale sand + hatched → `(anonymous
  namespace)::on_callback(…)` cool slate + outline → `[gpu:launch]` grey +
  outline → `[gpu:kernel:_Z14perfagent_axpy…]` green. At the bottom,
  `[gpu:launch unsampled]` is pale grey, hatched and dashed under the two wide
  green kernel bars.
- Sampled the rendered pixels rather than trusting the eye: every frame's
  colour matches its token exactly (`#82d38d`, `#d6d6d6`, `#bcc3cd`,
  `#f6de6a`, `#f3a0c2`, `#eda3a1`, `#d2cfcb` in light; the dark-theme values
  from the table in dark).
- Rendered the same page in **Firefox** headless and compared: identical. The
  `hsl(H calc(S% * var(--ds)) calc(L% + var(--dl)))` construction that the
  theme knobs depend on works in both engines.
- Rendered a degenerate profile (zero samples). The explanatory panel is
  intact in both themes: "No flame graph was drawn", the zero-samples
  sentence, "This is the profile reporting on itself, not a rendering
  failure", the `total: nothing to draw` chip, no `<svg>`, no `<script>`.
- The `gpu_sample_period=8` note, the unsymbolized-frame counts, the
  93.4%-under-`[gpu:launch unsampled]` note and the sample-labels table all
  still render.

## Cannot verify

- **That the palette is Gregg's.** I mapped it from the layer list in the
  brief (pink / red / yellow / orange / grey / aqua / green, with C, C++ and
  kernel in that order) and from his long-standing mixed-mode convention. I
  did not fetch https://www.brendangregg.com/blog/2024-10-29/ai-flame-graphs.html
  and did not sample pixels from his published graphs, so the *hues* are the
  right layers but the exact saturations and lightnesses are mine, chosen to
  clear the contrast floor rather than to match his image.
- **Rendering outside Chrome and Firefox.** Both were checked. Safari and
  WebKit were not, and no browser older than the ones installed here was. The
  page depends on `hsl()` with space-separated arguments, `calc()` inside it,
  and custom properties — all long-baseline, none tested below current
  versions.
- **The dark theme in a real user's browser.** Chrome's dark rendering was
  produced with `--blink-settings=preferredColorScheme=0`, and Firefox was
  only screenshotted light. I read the dark page as an image, not on a
  monitor; perceived glare or muddiness in the three near-greys is a judgement
  a screenshot can support but not settle.
- **Legibility of the hatched frames at real sizes under a real hatch.** The
  4.5:1 floor is computed against a fully-covering hatch, which is stricter
  than what is drawn; I did not measure the contrast of an actual rendered
  glyph pixel against the pixel behind it.
- **That no other consumer depended on the removed `fill=` attributes.**
  `RenderSVG` is exported and its output shape changed (colour now arrives via
  a `<style>` inside the `<svg>` rather than per-rect attributes). Nothing in
  this repository calls it outside tests, but an external consumer that
  post-processed the SVG by reading `fill=` would break.
