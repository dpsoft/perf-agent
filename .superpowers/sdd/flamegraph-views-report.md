# Flame graph: invert, tree view, and a search worth the name

Branch `feat/flamegraph-views`, one commit on top of `main` (which now
contains #75). Everything is in `internal/flamegraph/`; no other package is
touched.

## What async-profiler's Invert actually does

**It is a visual flip. It is not a reverse merge.** From `src/res/flame.html`:

```js
document.getElementById('inverted').onclick = function() {
    inverted = !inverted;
    render();
}
```

and the only thing `inverted` reaches, in the whole file, is the y of a row
and the y of the hit test:

```js
const y = inverted ? h * 16 : canvasHeight - (h + 1) * 16;
```

`levels[]` — their frame array — is never rebuilt, never re-bucketed, never
re-summed. Pressing `I` moves the picture, not the data.

The callee-centric aggregation the brief asked about **does exist in
async-profiler, and it is a different feature**: the `--reverse` flag on
their JFR/collapsed converter. `src/converter/one/convert/FlameGraph.java`:

```java
public void addSample(CallStack stack, long ticks) {
    Frame frame = root;
    if (args.reverse) {
        …
        for (int i = stack.size; --i >= skip; ) {
            frame = addChild(frame, stack.names[i], stack.types[i], ticks);
        }
    } else {
        for (int i = args.skip; i < stack.size; i++) { … }
    }
```

That walks each stack backwards as it is inserted, so leaves become roots and
hot leaves merge across call paths — a genuinely different tree, built in
Java before the HTML exists. The two features meet in exactly one line:

```java
// inverted toggles the layout for reversed stacktraces from icicle to flamegraph
out.print(args.reverse ^ args.inverted);
```

A reversed graph ships with `inverted` preset to true (so it draws downward,
which is the conventional rendering for a callee-centric view), and the `I`
key flips it back. `--reverse` decides *what tree*; `I` decides *which way up*.

**So I implemented the flip**, as instructed ("match their semantics"). I did
not build reverse-merge: it is not what `I` is, and it is a build-time
transformation of the fold, not a client-side view. If it is wanted it
belongs in `internal/foldedstacks`, beside the fold, as a second `Result` —
which would also let it keep the honesty notes straight, since a reverse
merge makes `[gpu:launch unsampled]` a *leaf* under every kernel and the
sample-period note would need rewording.

### How the flip is done here

Ours is a DOM, not a canvas, so there is no `render()` to call with a
different y. The rows used to be `.d3{bottom:54px}`. They now carry a
**number** and the frame turns it into an offset:

```css
.frame{bottom:calc(var(--d)*18px)}
.inv .frame{bottom:auto;top:calc(var(--d)*18px)}
.d0{--d:0}.d1{--d:1}…
```

`toggleInvert` is `chart.classList.toggle("inv")` and one `aria-label`
rewrite. Nothing else in the script knows the graph inverted. Three
consequences worth stating:

- **It is cheaper than what it replaced.** `.d16{--d:16}` is 12 bytes where
  `.d16{bottom:288px}` was 18; the ladder for this profile went 289 B → 268 B,
  and the gap widens with depth (`.d59{--d:59}` vs `.d59{bottom:1062px}`).
  The two positioning rules cost 76 B, so the icicle is +55 B here and
  *negative* on anything deeper than ~24 frames.
- **The row pitch survives.** It is still one px literal in a stylesheet.
  Measured in Chrome at 800/1280/1990 px, inverted: `font-size` 11 px, frame
  height 17 px, pitch 18 px — the same three numbers as the flame view, at
  every width.
- **It is lossless.** Verified by reading every frame's `style.left` and
  `style.width` before `I`, after `I`, and after `I` again: identical
  strings. Widths, values, zoom state and search state are untouched because
  nothing writes them.

`aria-label` on the chart becomes `"Flame graph, gpu/nanoseconds, inverted:
root at top"` while inverted, and reverts. A reader who cannot see the flip
is told about it.

## The tree view (`T`)

### Markup and semantics

```html
<div id="tree" role="group" aria-label="Call tree, gpu/nanoseconds" hidden>
  <ul>
    <li><details open><summary>
          <span class="p">100.00%</span>
          <span class="v">5.99 ms</span>
          <span class="sw" data-domain="root" aria-hidden="true"></span>
          <span>all</span>
        </summary>
        <ul><li>…</li></ul>
    </details></li>
  </ul>
</div>
```

A leaf is the same row in a `<div class="leaf">` instead of a `<summary>`,
inside the same `<li>`.

**I chose a nested list of native disclosures over `role="tree"`, and that is
the one decision here worth arguing about.** `role="tree"` /
`role="treeitem"` / `aria-expanded` is the textbook answer and I rejected it:
a tree role promises the reader a widget interaction model — one tab stop,
arrow keys to move, left/right to collapse and expand, type-ahead — and a
`role="tree"` that does not implement roving `tabindex` and arrow keys is a
worse outcome than no role at all, because the reader is told to expect
navigation that is not there. What `<ul>` + `<details>` buys instead, for
free and correctly:

- **list semantics** — a screen reader announces "list, 2 items" and the
  nesting depth, which is the structure this view exists to convey;
- **real disclosure semantics** — a `<summary>` *is* a button that announces
  expanded or collapsed, and the browser maintains that state. There is no
  `aria-expanded` for the script to desync;
- **keyboard for free** — Tab reaches a row, Enter/Space opens it, and
  `#tree summary:focus-visible` gives it a visible ring;
- **it matches the precedent this branch inherited.** #75 made the info panel
  a native `<details>` for exactly this reason. A second, hand-rolled
  disclosure with hand-maintained ARIA beside it would have been two answers
  to one question.

The swatch is `aria-hidden` — the colour is redundant with information the
row already carries, and "square, square, square" down a 60-row tree is
noise. The percentage and value are plain text in the row, so they are
announced.

### How it is built

**From the frames already in the document.** `writeNode` emits frames in
pre-order and each carries its depth as a class, so depth plus document order
is a parent link — one stack-walk at start-up reconstructs the whole tree.
The consequences:

- **The tree costs 61 bytes of markup**, the empty container. Not one symbol
  is on the page twice, which on a real profile is the difference between a
  30 KB page and a 60 KB one.
- Rows are built when their parent first opens (hooked on the `toggle`
  event), so a 20,000-frame profile builds the rows someone actually looked
  at. This is async-profiler's `expandTree` behaviour.
- A frame with **exactly one child opens all the way through**, theirs does
  the same, and it is not a nicety here: this profile's CPU path is a
  corridor fourteen frames long with no doors off it, and clicking fourteen
  times down it is not navigation. `_start` opens to sixteen rows in one
  click.
- Children are sorted by value descending — the tree's question is "what is
  under this", and the answer wants the big things first. The *markup* stays
  sorted by name, so the page is still byte-identical between renders.

The tree is rooted at the current zoom target, so the two views agree about
what you are looking at. Pressing `T` after zooming into `_start` gives a
tree of `_start`.

`title` on the row carries name, value, share, **self time**, module and the
inference notes — the native tooltip is right here, where #75's objection to
`<title>` (a second tooltip behind the cursor tooltip) does not apply,
because the tree has no cursor tooltip. Self time is new and is also now in
the flame graph's tooltip.

Colour: the swatch takes its fill, hatch and outline from the same rules the
frames use. That required dropping the `.frame` prefix from the eleven
`[data-domain="…"]` rules — a domain is a domain wherever it is drawn — which
**saved 84 bytes** and means an unsymbolized frame's swatch is hatched
without a second rule saying so. A test asserts none of those rules is
frame-scoped any more, so a tree swatch can never lose its colour to a
selector tightening.

## Search

Kept: `Ctrl+F` to reveal and focus, live incremental matching, `Esc` to clear.

Added:

- **A share of the total**, beside the count and the value:
  `3 matched · 5.99 ms · 100.00%`.
- **`N` / `Shift+N`** to step, with `(k of n)` appended to the counter, the
  status bar naming the frame, and — in the flame view — a zoom onto the
  match plus a scroll to its row, which is how you reach a 3-px sliver. In
  the tree view, `N` expands the path to the match and scrolls to it.
  Because the field swallows `N`, **Enter and Shift+Enter step from inside
  it**, which is the find-bar idiom.
- **A colour reserved to search.** A match is filled magenta, non-matches
  dim, and the one match `N` is standing on gets the ink outline that used to
  be on every match. (Every match outlined was wrong once matches are also
  filled: at 1.8 px inset on a 3-px frame the outline *is* the frame, and it
  would have painted a matched sliver black rather than magenta.)

**The share is a share.** Only the *outermost* matches are accumulated: a
match inside a match is highlighted, but its value is not added twice, so the
percentage cannot exceed 100. This is async-profiler's `totalMarked()`
dedupe, done as a single pre-order sweep because our items are already in
pre-order. `nav` is that same set, so `N` never walks you to a frame nested
inside the frame you are already looking at. The count `n` is every matching
frame and the value `v` is the outermost ones — two different questions, both
answered.

**The count is over the profile, not over the visible graph.** A count and a
share that changed when you zoomed would be answering a different question
from the one the reader asked. This also simplified `zoom()` and `reset()`,
which no longer re-run the search.

### The colour, and the contrast floor

async-profiler uses `#ee00ee`. **I did not**, because it fails the floor this
branch is required to hold. `#ee00ee` is `hsl(300 100% 46.7%)`; against
`--frame-ink` it is 4.99:1 bare, but **3.13:1 once the hatch is composited at
full coverage**, and a matched frame that is unsymbolized still carries its
hatch — losing it would be losing an honesty signal to a search highlight.

So the search fill is the same hue at the lightness the floor allows:

```css
--fill-match: hsl(300 100% 71%);
```

| | contrast vs `--frame-ink` |
|---|---|
| bare | **7.49:1** |
| hatched, light theme (α .26) | **4.56:1** |
| hatched, dark theme (α .20) | **5.15:1** |

70% gives 4.46:1 and fails; 71% is the first rung that passes. It is
**not exempt** and `TestTheSearchColourIsLegibleAndReservedToSearch` walks it
in both themes at both hatch alphas.

It is deliberately **outside** the themed `--fill-*` family — no `--fill-dl`,
no `--fill-ds` — for the same reason `--frame-ink` is: it is a signal, not a
layer, and a signal that shifted with the page would have to be re-proved
twice. The dark-theme block is asserted not to mention it. It is also off the
jitter ladder: `.frame.match` sets `background-color` directly, so a reserved
colour is exactly one colour. And it is in the legend, named as
"Not a domain", so the page says out loud that one of its colours means
something else.

The jitter guarantee is untouched — `worst(all rungs) == worst(rung 0)` still
passes, because nothing about the ladder or the fills moved.

## The keyboard map

| key | action |
|---|---|
| `I` | invert: root at top |
| `T` | tree view |
| `Ctrl+F` / `Cmd+F` | reveal and focus the search field |
| `N` / `Shift+N` | next / previous match (Enter / Shift+Enter inside the field) |
| `0` | reset zoom |
| `Esc` | close the panel, else clear and hide search, else reset zoom |
| `D` | light / dark |
| `?` | info panel |
| click a frame / the background | zoom / reset |

`I` was ours for the info panel and is theirs for invert. **Theirs wins**; the
panel is `?` alone and is also two clicks away on the ⓘ button, which is how
async-profiler reaches its legend. The panel's own Keys list is regenerated
from the same table, and a test asserts every key in that list is a key the
script actually handles — the panel is the only place these are discoverable,
so a map that drifted from the code would be worse than no map.

Two buttons joined the top bar, ⇅ (invert) and ☰ (tree), with `title`s naming
their keys, next to the existing ⓘ and ◐.

## The byte cost

Rendered from `/home/diego/gpu-cuda-45.pb.gz` (RTX 3090, 4,000 samples, 3
stacks, 20 frames, 17 depths). **23,369 B → 29,304 B, +5,935 (+25.4%).**

| block | before | after | delta | what arrived |
|---|---:|---:|---:|---|
| the graph | 4,304 | 4,304 | **+0** | the tree is built from these frames, so not one byte of the new views is per-frame markup |
| `<style>` | 7,823 | 8,937 | **+1,114** | treeCSS 1,062; the two icicle rules +76; `--fill-match`, `.frame.match`, `.frame.cur`, `.sw[data-domain]` +91. Against that, the depth ladder got 21 B shorter and dropping `.frame` from eleven domain selectors saved 84 |
| `<script>` | 4,867 | 8,964 | **+4,097** | the tree builder and its lazy/chain expansion (~2,050), reveal-in-tree and the nav/step machinery (~1,050), the parent-link and self-time pass (~450), invert (~180), the search rework (~370) |
| info panel | 5,499 | 5,918 | **+419** | three more key rows, one more legend row |
| chrome | 876 | 1,181 | **+305** | two buttons (244) and the tree container (61) |

**The script more than doubled and that is where the whole cost is.** A tree
view is a second renderer; it does not get cheaper than that while the brief
says keep the DOM. The one thing I would not trade for bytes is where the
tree's data comes from: emitting rows from Go instead would have been maybe
600 B of simpler script and **+3,900 B of markup on this profile alone**,
scaling with frame count, since every symbol would be on the page twice.

**The degenerate page is byte-identical, 6,805 B before and after** — `diff`
clean. That is why treeCSS is its own constant emitted beside `paletteCSS`
rather than living in `styleSheet`: a profile with nothing to draw has no
tree to press `T` for and should not carry 1,062 bytes of rules for one.
There is a test for it.

## Nothing regressed — checked, not assumed

- **Percentage geometry.** Chrome, `.chart` measures 776 / 1,256 / 1,966 px at
  800 / 1,280 / 1,990 px windows (window minus the 24 px body padding), and
  `documentElement.scrollWidth == clientWidth` at all three. `font-size`
  11 px, frame height 17 px, row pitch 18 px, **identical at all three widths
  and identical inverted**. The script still contains no `resize`,
  `clientWidth`, `offsetWidth` or `getBoundingClientRect().width`; the tests
  that assert this are unchanged and pass.
- **The no-JS baseline.** With the `<script>` element deleted, PNG
  screenshots are **byte-identical** (sha256) to the scripted page at 800,
  1,280 and 1,990 px. The default view is still the unscripted one: no `inv`
  class in the markup, chart not hidden, `#tree` empty and `hidden`. A new
  test pins all of that, and the existing no-JS tests pass untouched.
- **Accessibility.** Frames keep `role="img"` + `aria-label`; the chart keeps
  `role="group"`. The tree container gets `role="group"` and a name for the
  same reason. New semantics are argued above.
- **Jitter's lighten-only guarantee.** `worst(all rungs) == worst(rung 0)`
  still passes, unchanged.
- **The rest.** The one-line `gpu_sample_period` note, the status bar, the
  degenerate page, self-containment (no CDN, no external font — that test is
  unchanged), and `html.EscapeString` on symbol names are all as they were.
  The tree writes every string with `textContent`, never `innerHTML`.
- `go build ./... && go vet ./... && go test ./... -count=1` — all packages
  ok. `golangci-lint run --timeout=5m` — 0 issues.

One rule I broke and then fixed: my first draft put explanatory comments
*inside* the `script` constant, which assets.go's own header forbids —
"a comment inside one of these constants is shipped to every reader of every
page perf-agent writes". Moving them to Go comments saved **1,624 B**, and
`TestTheShippedAssetsCarryNoProseComments` now enforces the rule that was
previously only written down.

## Verification actually performed

Driven in headless Chrome 1280×900 over a hand-written CDP client
(`Input.dispatchKeyEvent`), so the keys below were **really pressed**, not
simulated with synthetic `KeyboardEvent`s:

- `data-total="5987854"` and the synthetic root at `data-value="5987854"`,
  before and after everything.
- `I` → root moves from the bottom row to the top, hatching, jitter and
  labels intact, `aria-label` updated. `I` again → every frame's `left` and
  `width` string identical to the start. **Lossless.**
- `T` → 3 rows, then 20 after expanding, matching the 20 frames exactly;
  chart hidden, tree shown; `T` again → geometry identical to before.
  Zoomed into `_start` then `T` → the tree is rooted at `_start`, 16 rows,
  the whole corridor auto-opened.
- `Ctrl+F`, typed `cuda` → `1 matched · 397.13 µs · 6.63%`, one magenta
  frame at `rgb(255,107,255)`, 19 dimmed. `gpu:kernel` → 3 matched, and
  `N` walked 1→2→3→1 and `Shift+N` walked back, zooming to each.
- The share cannot exceed 100: searching `a` (which matches the root)
  reports `15 matched · 5.99 ms · 100.00%`, not 700%.
- `N` in the tree view expands the path and marks the row; a match outside
  the tree's current root drops the tree back to the whole profile rather
  than silently doing nothing (this was a bug I found this way and fixed).
- `Esc` clears the field, hides it, clears the counter and drops every match
  class. `?` opens the panel, `Esc` closes it, and `I` **does not** open it
  any more — it inverts.
- Both themes: `D`, then all three views screenshotted. Tree swatches follow
  the dark knobs (`root` 210,207,203 → 189,184,178); the magenta and its ink
  do not move, by design.
- Firefox 150 headless: flame graph, icicle and tree + search all render
  correctly, including `calc(var(--d)*18px)`, the hatches and the magenta.

## What I could not verify

- **No Safari or WebKit, at all.** Chrome 1xx and Firefox 150, both headless,
  both on this machine. No Windows, no mobile. `hsl(from …)` and
  `calc(var(--d)*18px)` are both old enough that I expect Safari fine, but I
  did not see it.
- **Firefox was screenshotted, not driven.** I have no marionette client, so
  the Firefox icicle and tree were produced by pre-setting the state in the
  markup and by an appended bootstrap script — which exercises the CSS (the
  part that differs by engine) but **not one Firefox keypress**. `I`, `T`,
  `N` and `?` have only ever been pressed in Chrome.
- **No screen reader.** The tree's semantics are argued from the HTML-AAM
  mapping and from `<details>` being spec behaviour, not from listening to
  it. Nobody has run NVDA, JAWS, VoiceOver or Orca over this page. In
  particular I have not confirmed that a nested `<ul>` of `<details>` is
  navigable in practice at fourteen levels of nesting, which is what this
  profile produces — deep nesting is exactly where my "a list is honest
  enough" argument is weakest, and it is untested.
- **No real pointer and no real window.** Clicking a tree row to expand it,
  dragging a window edge to watch the icicle reflow, and scrolling a tree
  taller than the viewport were all verified by reading the code, by
  measuring at fixed widths, or by dispatching events — not by using them.
  `scrollIntoView` in particular has never been watched.
- **Only one profile.** 20 frames, 17 depths, 3 stacks. The tree's lazy
  build, the chain-expansion and the `N`-through-many-matches path are all
  designed for a 20,000-frame system-wide profile and **none of them has seen
  one**. I did not render one. The performance claim ("builds the rows
  someone looked at") is a property of the code, not a measurement.
- **The tree has no keyboard navigation of its own** beyond Tab. That is the
  deliberate consequence of not claiming `role="tree"`, but it means a
  keyboard user tabs through every visible row to reach the bottom of a deep
  tree. `N` is the only fast way down, and only when a search is active.
- **Icon glyphs depend on the reader's fonts.** ⇅ (U+21C5) and ☰ (U+2630)
  rendered correctly in Chrome and Firefox here; I cannot promise them on a
  machine without a font that covers them. Both buttons carry `aria-label`
  and `title`, so the meaning survives a missing glyph even if the picture
  does not.
- **Two dead buttons with JavaScript off.** ⇅ and ☰ do nothing on an
  unscripted page, as ◐ already did. Screenshots stay byte-identical because
  a button renders the same either way, so the pinned property holds, but it
  is a wart and I am naming it rather than hiding it.
- **Theme is still not persisted**, and the tree's expansion state is lost on
  `T`-off/`T`-on if the zoom root changed.
