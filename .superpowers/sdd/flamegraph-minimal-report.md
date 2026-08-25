# Flame graph: async-profiler's chrome discipline

Branch `feat/flamegraph-minimal-chrome`. One commit. Renderer is
`internal/flamegraph/`; outside it, `cmd/flamegraph` loses its `-width` flag
and `perfagent` has one test assertion updated.

## The graph fills the window

The page used to open with

```html
<svg class="flame" width="1280" height="322" viewBox="0 0 1280 322" …>
```

— a hard-coded 1280 px. On a 1990 px window that left ~700 px of dead space
and truncated `0x7f2c945b…` and `[gpu:kerne…` for want of room that was
sitting empty to the right.

**The geometry is percentages of the window now, not user units of a
viewBox.** A frame is an absolutely positioned `<div>` inside a `.chart` that
is an ordinary block, so it is as wide as the body, so it is as wide as the
window:

```html
<div class="chart" style="height:305px" role="group" aria-label="Flame graph, gpu/nanoseconds" data-total="5987854" data-unit="nanoseconds">
<div class="frame d1" style="left:0%;width:93.368%" role="img" aria-label="[gpu:launch unsampled], 5.59 ms, 93.37% of total" data-domain="boundary-unattributed" data-value="5590723">[gpu:launch unsampled]</div>
```

Measured in Chrome, `.chart` is 1966 px wide on a 1990 px window, 1256 px on
1280, 761 px on 800 — window minus the body's 12 px padding, every time —
and `documentElement.scrollWidth == clientWidth` at 400, 800, 1280, 1990 and
3000 px, so nothing overflows either way.

**Nothing scales.** This is the whole reason `width="100%"` on the old
viewBox was not the fix: at 1990/1280 it would have magnified everything
1.55×, turning 11 px labels into 17 px ones and 18 px rows into 28 px ones.
That is a zoomed picture, not a filled one, and it buys no extra frames.
Under percentage geometry the only px dimensions left are in the stylesheet,
so they cannot vary with the window: `font-size` is 11 px, frame height and
line height 17 px, row pitch 18 px, measured identical at 800, 1280 and
1990 px in both themes and after a zoom. The graph gets *wider*, not bigger,
which is what buys the legibility: at 1990 px the seven hex vendor frames and
`[gpu:kernel:_Z14perfagent_axpyfPKfPfi]` now render **in full**, where the
1280 px SVG cut them.

**No script is involved, at any width.** There is no resize handler and no
layout pass — a test asserts the script contains no `resize`, `clientWidth`,
`offsetWidth` or `getBoundingClientRect().width`, and another strips
everything from `<body>` to `<script>` and checks every frame still carries
its span. The reflow is the browser resolving a percentage twice. This was a
requirement rather than a nicety: the branch had just made the info panel a
native `<details>` so the legend survives with JavaScript off, and a graph
that needed a resize handler to be the right width would have handed that
principle straight back. Screenshots of the page with the `<script>` element
deleted are byte-identical to the full page at 800, 1280 and 1990 px.

### Truncation is CSS now

The renderer used to measure: `data-min-text`, `data-char-width=6.4`, a Go
`truncate()` and a JS `fit()` that cut the label to `floor((w - 8) / 6.4)`
characters. It cannot do that any more — it knows a frame is 6.632% wide and
has no idea what that is in pixels — and it should never have been doing it,
because 6.4 px was a guess at the advance of a font chosen by the reader's
machine.

So the markup carries **the whole symbol** and the stylesheet cuts it:
`overflow:hidden; white-space:nowrap; text-overflow:ellipsis` on the frame,
with 4 px of horizontal padding. The browser cuts at the reader's real width
in the reader's real font, and never splits a rune — the guarantee
`TestTruncateNeverSplitsAMultibyteRune` used to hold now belongs to the text
engine, and that test is gone with the function.

Consequences worth stating:

- **`data-name` is gone.** The frame's text *is* its name, in full, so a
  `data-name` would have been a second copy of an arbitrarily long string on
  every frame — exactly the duplication this branch removed from
  `data-orig-x`. The script reads `textContent`. Verified on a truncated
  frame: the label reads `cudaLaunchKer…` while the tooltip and status bar
  say `cudaLaunchKernel`.
- **Below ~14 px a frame shows a lone ellipsis, and below 8 px nothing** —
  the content box is zero once the padding is subtracted. That is the browser
  applying the rule rather than a threshold anyone picked; the old
  `minTextWidth = 24` was such a threshold and it was equally arbitrary.

### What else the geometry change moved

- **Hatching.** The two SVG `<pattern>`s and the extra `<rect>` per hatched
  frame are two `repeating-linear-gradient` tokens, `--hatch-gap` and
  `--hatch-inf`, applied as `background-image` by `data-domain` and by
  `.frame.inexact`. They run opposite diagonals, as the patterns did, and a
  frame that is both unnamed and inferred gets both layers — there is a test
  for each hatched domain having a rule and for the combined case, because a
  hatch is an honesty signal and losing one to the cascade would be silent.
  `DomainInfo.Overlay` is `var(--hatch-gap)` where it was `url(#p-gap)`.
- **Outlines are painted, not laid out.** `stroke` became inset `box-shadow`
  rather than `border`, because with `box-sizing:border-box` a 1 px border
  each side would eat the whole content box of a 3 px sliver. The one dashed
  outline (`[gpu:launch unsampled]`) uses `outline` with `outline-offset:-1px`,
  which box-shadow cannot dash and which is also not laid out. A test walks
  every `.frame` rule and fails on `border:`.
- **Rows are a class, not an inline `top`.** `rowCSS(maxDepth)` emits
  `.d0{bottom:0px}…`, 289 B here, against 12 B per frame inline — a profile
  has far more frames than depths. Measured from the *bottom* so a depth
  means the same offset whatever the profile's height.
- **`RenderSVG` is now `RenderFragment`**, and emits the same divs plus the
  palette. Keeping an SVG renderer alive for embedding would have meant a
  second geometry engine, a second palette (`fill`/`stroke` beside
  `background`/`box-shadow`) and a second truncation rule, and they would
  have drifted. It had no callers. It still refuses a degenerate profile,
  which was the property worth keeping.
- **`Options.Width` and `flamegraph -width` are gone.** There is no width to
  set: the page fills what it is opened in.

## Size

Rendered from `/home/diego/gpu-cuda-45.pb.gz` (RTX 3090, 4000 samples, 3
stacks, the 16-frame joined stack with 7 hex vendor frames):

| | before the branch | chrome discipline | + percentage geometry |
|---|---|---|---|
| GPU profile page | **34,736 B** | 27,000 B (−22.3%) | **23,369 B** (−32.7%) |
| degenerate page | 9,614 B | 6,927 B | 6,944 B (−27.8%) |

The geometry change is −3,631 B on its own (−13.4% against 27,000 B), and
the degenerate page pays +17 B for it — that page has no graph, so all it
sees is the stylesheet swapping three `svg.flame` rules for two `.frame`
ones. Where the 3,631 B went:

| block | chrome discipline | + geometry | what happened |
|---|---:|---:|---|
| the graph | 7,586 | 4,304 | `<svg>`/`viewBox`/`xmlns` and five geometry `data-*` attributes gone; `<g>`+`<rect>`(+`<rect>`)+`<text>` per frame became one `<div>`; two coordinates per frame instead of six; `data-name` gone. Against that, every label is now emitted in full instead of pre-truncated |
| `<style>` | 7,198 | 7,823 | the frame box, the two hatch gradients and 289 B of row rules arrive; the SVG `<defs>`, the pattern rules and `svg.flame text` leave |
| `<script>` | 5,762 | 4,867 | `fit()`, the per-rect placement loop and five `data-*` readbacks gone; `place()` is two assignments |

Total conserved: `data-total="5987854"` and the synthetic root at
`data-value="5987854"`. `5.99 ms` on the status line, `gpu/nanoseconds` named
as the axis.

Where the 9.3 KB went, and where the remaining 25.4 KB is:

| block | before | after | what happened |
|---|---:|---:|---|
| the graph | 14,476 | 7,586 | `data-path` (3,764 B — the breadcrumbs' backing store, O(depth²)), per-frame `<title>` (2,299 B — replaced by `role="img" aria-label`, 1,520 B, below), `data-orig-x`/`data-orig-width` (916 B — read back off the rects instead), `data-domain-label` (777 B), always-zero `data-inexact` (336 B), never-read `data-depth` (287 B) |
| `<style>` | 8,021 | 7,198 | toolbar/chip/breadcrumb rules gone; 1,373 B of CSS comments moved into Go comments; status bar, tooltip, info panel and jitter ladder added; the degenerate page's own CSS is now emitted only on the degenerate page |
| `<script>` | 5,145 | 5,762 | lost breadcrumbs and the zoom chip, gained the tooltip and the theme toggle; the panel's open/close is native |
| chrome above the graph | 1,947 | 372 | header + chips + toolbar + notices box → one title line with two icons and one note |
| legend + labels + keys + provenance | 4,534 | 5,486 | same content plus the keyboard map, now inside `<details id="info">` |

**This is not 15–20 KB and I did not force it there.** async-profiler reaches
that with a canvas plus a compact JS array where we have 20 DOM frames (the
brief says keep DOM), and with an info panel of a few lines where we carry
5.4 KB of legend, honesty notes, label report and provenance. That 5.4 KB is
now invisible until asked for, which was the actual complaint; deleting it to
hit a number would trade a real property for a smaller file. The remaining
un-minified 13 KB of CSS+JS is legible in View Source, which I judged worth
more than the ~4 KB minification would save.

### Accessible names

Every frame group carries `role="img"` and an `aria-label` of name, value and
share — the same three things, in the same order, that the status bar shows,
so hearing the page and seeing it agree. **1,520 B for 20 frames, 779 B
cheaper than the 2,299 B of `<title>` it replaces**, because the label drops
the domain, module and inference lines that the visual tooltip still carries.

`aria-label` rather than restoring `<title>` for the reason the coordinator
gave: a `<title>` is both the accessible name and a native browser tooltip,
so it would put a second tooltip on a one-second delay behind the cursor
tooltip on every hover. `role="img"` goes with it because without a role the
frame's text child is exposed separately and a reader hears the truncated
on-screen label ("0x7f2c945b…") after the real name; `img` makes the subtree
presentational, so the frame is announced as one object. That mattered more
once the geometry moved to HTML, because the text child is now the *whole*
symbol and the truncation is visual only.

The container is named too — "Flame graph, gpu/nanoseconds" — but with
**`role="group"`, not `role="img"`**. This is the one place the SVG→HTML
translation is not mechanical: an `<svg>` with an `aria-label` maps to
`graphics-document` and keeps its children exposed, whereas `role="img"` on a
`<div>` would make the entire subtree presentational and silence all twenty
frames. `group` takes an accessible name and leaves its children alone. Escaping is the existing
`html.EscapeString`, tested against `Foo<Bar&"Baz">::run()`.

### The info panel opens without JavaScript

`<details id="info"><summary id="info-btn">ⓘ</summary><div id="panel">…` —
the disclosure is native, so a page opened with JavaScript off, or saved and
reopened somewhere that blocks it, can still reach the legend, which used to
be permanent. It renders collapsed, so "nothing permanent explains the page"
holds. Verified by deleting the `<script>` element from the rendered page
entirely and screenshotting: graph, note, status line and panel all correct.
`<summary>` also brings disclosure semantics a `<div hidden>` never had — it
is a button that announces expanded or collapsed. The `?` / `I` key, `Esc`
and click-outside now set `info.open` rather than toggling a `hidden`
attribute; a test asserts the script contains no path that sets the panel's
own visibility, so the no-JS route cannot rot.

## Notes: what stayed, what moved

**Stayed on the page, one line, always visible** — `gpu_sample_period`:

> gpu_sample_period=8 — one launch in 8 carried a CPU stack, so the width
> under [gpu:launch] is a sample of launches, not that path's share of GPU
> time. Nothing is scaled by 8.

Four sentences to one, 206 B, one line at 1400 px. It keeps all three facts:
the ratio, the misreading it prevents, and that no value is scaled. It stays
because it is the only note whose content is *absent from the picture* —
nothing about the drawing hints that launches were subsampled. The long form
is suppressed from the panel so the note is never made twice.

**Moved into the info panel:**

- *"1799 of 11598 frame slots (15.5%) have no symbol…"* — the frames it
  describes are on screen, hatched, named by their address, and say
  "no symbol: the unwind found this frame, nothing could name it" on hover.
  The note aggregates what the graph already shows.
- *"93.4% of the total sits under [gpu:launch unsampled]…"* — 93.4% **is**
  the width of that frame. The note restates the picture in numbers.
- Zero-value samples, empty stacks, multi-axis profiles — all statements
  about what was excluded, none of them a trap.

So the page does not hide that it has caveats: **the info button carries an
orange dot whenever the profile produced notes** (`#info-btn.notes::after`),
and the dot is absent on a clean profile. Both cases are tested.

**Degenerate page** — unchanged in content. Diffed against the old renderer:
the only differences are the `<main>` wrapper (replaced by a `max-width` on
`body`) and the `hdr` class. Still no `<script>`, no `color-mix`, no
`hsl(from` — and now no `class="chart"` either, where the assertion used to
be no `<svg>`. Re-screenshotted after the geometry change: chips, notices,
"No flame graph was drawn", label table and footer all correct.

## Jitter, and how it was bounded

`assignJitter` in `render.go`; ladder in `assets.go`; contrast in
`palette_test.go`.

**Direction is forced, not chosen.** The worst label contrast on the page is
4.605:1 (dark theme, `--fill-system`, hatch composited at full coverage) —
0.105 above the 4.5:1 floor. I measured what darkening costs before writing
any of it:

```
jitter     0%  worst 4.605:1     jitter -2%  worst 4.326:1
jitter    -1%  worst 4.463:1     jitter -3%  worst 4.194:1
```

−1% already breaches the floor. So jitter **only ever lightens**, exactly as
async-profiler's `getColor()` only ever adds to a base. Raising lightness at
fixed hue and saturation raises luminance monotonically, so the worst-case
jittered frame is the *un-jittered* one and the floor does not move. The test
asserts this rather than assuming it: it walks every rung of every token in
both themes, requires each rung to be strictly lighter than the last, and
requires `worst(all rungs) == worst(rung 0)`. It also fails if the floor ever
grows past 4.7 — at which point the jitter could afford to be bolder.

**Mechanism.** A first attempt used `color-mix(in srgb, #fff p%, fill)`, the
literal translation of async-profiler's "add N to each channel". It is nearly
invisible on this palette: our fills are already light, so 6% white moves
`--fill-unsym` three units per channel. Switched to a lightness delta in
relative colour syntax:

```css
.frame{background-color:var(--fill-x);background-color:hsl(from var(--fill-x) h s calc(l + var(--fill-j)))}
```

+6 lightness moves the same fill fifteen units per channel. Hue and
saturation never move, so the palette still reads. Verified in **both Chrome
and Firefox** that `l` resolves as a bare number (`calc(l + 6%)` renders
white in both — a percentage there is wrong) and that the result is
pixel-identical to the hand-written `hsl()`. The plain `fill:var(--fill-x)`
is declared first: an engine without relative colour syntax (pre Chrome 119 /
Safari 16.4 / Firefox 128) drops the second declaration and keeps the
palette. (When the fills were SVG `fill:` the failure mode was a page of
black rectangles; on a `<div>` it is a page of transparent ones, which is no
better.)

**Amplitude: 8 lightness points, 8 rungs.** Palest jittered fill
`--fill-unsym` at 77% tops out at 85% — still sand. Not white.

**The token layout survives untouched.** Every colour is still one `hsl()` at
`:root`; the frame reaches it through `--fill-x` and composes the jitter in
its own `background-color:` property. This is the only arrangement that works: `var()`
inside a custom property is substituted when *that property* is computed, on
the element that declares it, so a `--fill-app` referencing `--fill-j` would
freeze in `:root`'s value of it.

**Two domains sit out of the ladder**: `boundary` and
`boundary-unattributed`, plus the synthetic root. Their lightness difference
is a *meaning* — paler means nothing behind it, and there is a test enforcing
the 5-point gap — and a random lightness must not compete with a meaningful
one. (`--fill-boundary-unattributed` is also at 92% and would reach 100.)

**Better than random where it counts.** async-profiler's is `Math.random()`
per frame and may collide with the neighbour it was meant to separate. Ours
hashes the frame name (so a render is byte-identical to the last one and a
frame keeps its shade across a zoom), then walks the tree with the parent and
previous sibling in hand and steps away from both by at least 3 rungs (+3.4
lightness). The seven consecutive hex frames now land on rungs
2, 6, 1, 7, 1, 4, 1 — every touching same-domain pair separated by
construction. Tested.

## Keyboard map

| key | action |
|---|---|
| `Ctrl+F` / `Cmd+F` | reveal and focus the search field (bottom right) |
| `0` | reset zoom |
| `Esc` | close the info panel, else clear and hide search, else reset zoom |
| `D` | light / dark, overriding the OS in both directions |
| `?` or `I` | info panel |
| click a frame / the background | zoom / reset |

Discoverable from the panel's first section, and from the two buttons'
`title` attributes. `Ctrl+F` shadows find-in-page, as async-profiler's does.

## Other changes

- **One bottom status bar.** Resting content is the profile summary
  (`5.99 ms total · 4,000 samples · 3 stacks · gpu/nanoseconds`) — the chip
  row's numbers keep a permanent home without a chip row. On hover it becomes
  `name · value · share`. Match counter bottom right
  (`5 matched · 1.99 ms · 33.16%`), with the search input beside it.
- **Tooltip at the cursor**, flipping at the viewport edges, carrying name,
  value, share, module (only when the profile has one) and the
  inference/no-symbol notes. Built with `textContent`, never `innerHTML`.
- **Theme toggle.** `--fill-dl` / `--fill-ds` / `--hatch-ink` and the ten
  chrome tokens are each written **once** as a Go constant and emitted twice:
  under `@media (prefers-color-scheme: dark){:root:not([data-theme="light"])}`
  and under `:root[data-theme="dark"]`. Two copies of one constant cannot
  drift, and a test asserts the two blocks are byte-identical and that
  neither declares anything but the knobs. Verified pixel-identically: the
  OS-dark render and the `data-theme="dark"` render hash the same, as do
  OS-light and `data-theme="light"`-under-dark-OS.
- Aqua still reserved, unused, and tested. Escaping unchanged. No network,
  no CDN, no external font — the existing test still passes.

## What I could not verify

- **The graph was measured at five widths, not resized at one.** Headless
  Chrome cannot resize a window, so "reflows on resize" is verified by
  loading the same file at 400/800/1280/1990/3000 px and measuring, plus by
  reading the CSS: nothing in the layout is in pixels, so there is nothing
  for a resize to invalidate. **Nobody has dragged a window edge and watched
  it reflow.** The same applies after a zoom: I confirmed the zoomed frames
  are still expressed as percentages (`left:0%;width:100%` for the zoom
  target) and inferred that a resize therefore reflows them, rather than
  seeing it.
- **The ellipsis band was reasoned about, not photographed.** "Nothing below
  8 px, a lone ellipsis to about 14 px" follows from 4 px of padding either
  side and the advance of `…`; this profile's narrowest frame is 130 px at
  1990 px and 51 px at 800 px, so no frame on the verification page is
  anywhere near the band. A dense system-wide profile would be the test, and
  I did not render one.
- **Only Chrome and Firefox, both headless, both current, both on this
  machine.** No Safari or WebKit at all — and Safari is the engine where
  `hsl(from …)` has the longest history, so I expect it fine, but I did not
  see it. No Windows, no mobile.
- **Sub-pixel `box-shadow`.** The default frame edge is
  `inset 0 0 0 .5px`, which is what the old `stroke-width:.5` amounted to.
  Chrome and Firefox both render it as a faint hairline; a half-pixel inset
  shadow is a place engines are entitled to differ, and I checked two.
- **No real pointer.** Hover, tooltip placement and search were exercised by
  dispatching synthetic `mouseenter`/`mousemove`/`input` events and
  screenshotting the result. I never clicked a frame with a mouse, so
  click-to-zoom, click-outside-to-close and the keyboard handlers are
  verified by reading and by unit tests, not by using them.
- **The theme toggle's runtime behaviour** was verified by rendering the two
  end states (`data-theme` set in the markup), not by pressing `D`.
- **No screen reader.** Every frame now has `role="img"` and an `aria-label`,
  and the `<summary>` brings native disclosure semantics, but I verified this
  by reading the markup and the HTML-AAM mapping rules, not by listening to
  it. Nobody has run NVDA, JAWS, VoiceOver or Orca over this page. In
  particular I have not confirmed that `role="img"` on a `<div>` actually
  suppresses its text child in every reader, which is the whole reason the
  role is there, nor that `role="group"` on the container leaves the frames
  exposed in every reader — and the frames now carry their full symbol as
  text, so if `role="img"` is ignored the double-announcement is longer than
  it used to be.
- **The no-JS path was verified by deleting the `<script>`, not by disabling
  JavaScript in a browser** — this time including the layout, whose
  screenshots came out byte-identical to the scripted page at all three
  widths, which is the strongest form the check takes here —, and the native `<summary>` toggle was verified
  by forcing the `open` attribute rather than by clicking it — headless
  screenshots cannot click. The toggle itself is HTML-spec behaviour, not
  code of mine, but I did not watch it happen.
- **Icon glyphs depend on the reader's fonts.** `&#9432;` rendered as a
  slightly squarish circled *i* in this machine's fallback font and `&#9680;`
  rendered correctly, but I cannot promise either on a machine without them.
- Theme choice is not persisted (no `localStorage`); `D` resets on reload.
- Jitter separation is guaranteed against the parent and the previous
  sibling. Two same-domain frames that touch only diagonally, or across a
  zoom-induced reflow, may still land close together.
