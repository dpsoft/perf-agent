package flamegraph

import (
	"fmt"
	"strings"
)

// Everything in this file is inlined verbatim into the output page. Nothing
// here may reference an external URL: the deliverable is one file that
// renders correctly from file:// with no server and no network.
//
// No user- or symbol-derived text is ever placed inside <style> or <script>.
// All profile data reaches the page through escaped HTML attributes and text
// nodes, and is read back with dataset/textContent, so a mangled C++ name
// full of <, > and & cannot inject markup or script.
//
// The prose that explains these rules lives in Go comments, not in CSS or JS
// comments. A comment inside one of these constants is shipped to every
// reader of every page perf-agent writes; the audience for "why aqua is
// reserved" is whoever edits this file.
//
// Chrome discipline, borrowed from async-profiler's src/res/flame.html:
// nothing permanent on the page explains the page. The graph, one line of
// status, and — only when the profile carries the trap it guards against —
// one line of warning. The legend, the keyboard map, the notes and the label
// table are all one keypress (or one icon) away.

// darkChrome is the page chrome's dark theme, written once and emitted
// twice: once behind prefers-color-scheme for readers who never touch the
// D key, once behind [data-theme="dark"] for readers who do. Two copies of
// one Go constant cannot drift; two copies of ten hex literals could.
const darkChrome = `--bg:#16151a;--panel:#1e1d23;--ink:#eceaf2;--muted:#a29caf;--line:#332f3b;` +
	`--accent:#ff9a6a;--warn-bg:#2b2313;--warn-line:#6d5722;--fatal-bg:#2e1616;--fatal-line:#7a3232`

// styleSheet is the page chrome — the title line, the two icon buttons, the
// one-line note, the fixed status bar, the cursor tooltip and the info panel
// — plus the frame box itself.
//
// The frame box is the whole of the layout engine. A frame is an absolutely
// positioned block whose left and width are PERCENTAGES of .chart, which is
// itself an ordinary block and therefore as wide as the window minus the
// body padding. That is why the graph fills any viewport and reflows on
// resize with no script: the browser resolves the percentages, and it
// resolves them again when the window changes. Height, padding, font size
// and row pitch are all in px and never scale, so a frame at 1990 px and the
// same frame at 800 px carry the same 11 px label — the graph gets wider,
// not bigger. (The SVG this replaced had to pick a width, and did: 1280,
// baked into a viewBox. Setting width="100%" on that viewBox would have
// scaled the text with it, which is zoom, not fill.)
//
// Truncation is the browser's now, not the renderer's. overflow:hidden with
// text-overflow:ellipsis cuts the label to whatever the frame can hold, at
// the reader's actual window width, in the reader's actual font — three
// things a Go function measuring characters against a nominal 6.4 px advance
// could only guess at. Frames narrower than the 8 px of horizontal padding
// have a zero-width content box and draw no text at all; between there and
// about 14 px they draw a lone ellipsis. Both are the browser applying the
// rule, not a threshold anyone chose.
const styleSheet = `
:root{--bg:#fbf9f6;--panel:#fffefc;--ink:#1d1a17;--muted:#6b6259;--line:#e2dad0;--accent:#b4522a;--warn-bg:#fff6e8;--warn-line:#e8c88a;--fatal-bg:#fdecec;--fatal-line:#e0a3a3}
@media (prefers-color-scheme: dark){:root:not([data-theme="light"]){` + darkChrome + `}}
:root[data-theme="dark"]{` + darkChrome + `}
*{box-sizing:border-box}
body{margin:0;padding:8px 12px 30px;background:var(--bg);color:var(--ink);font-family:ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:13px;line-height:1.5}
code,kbd{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11.5px}
.muted{color:var(--muted)}
.top{display:flex;align-items:baseline;gap:8px}
h1{font-size:13px;font-weight:600;margin:0;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.top button,.top summary{cursor:pointer;color:var(--muted);background:none;border:0;padding:0 2px;font-size:15px;line-height:1;list-style:none}
.top summary::-webkit-details-marker{display:none}
.top button:hover,.top summary:hover{color:var(--accent)}
#info-btn.notes::after{content:"\2022";color:var(--accent);vertical-align:super;font-size:11px}
.note{margin:6px 0 0;padding:3px 9px;border:1px solid var(--warn-line);border-left-width:3px;border-radius:5px;background:var(--warn-bg);font-size:12px}
.chart{position:relative;margin:8px 0 0}
.frame{position:absolute;height:17px;padding:0 4px;border-radius:2px;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;line-height:17px;cursor:pointer}
.frame.dim{opacity:.15}
#status{position:fixed;left:0;right:0;bottom:0;z-index:6;display:flex;gap:12px;align-items:center;padding:2px 12px;background:var(--panel);border-top:1px solid var(--line);font-size:12px}
#st{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
#mc{color:var(--muted);white-space:nowrap}
#q{font:inherit;color:inherit;background:var(--bg);border:1px solid var(--line);border-radius:5px;padding:1px 6px;width:15em}
#tip{position:fixed;z-index:8;left:0;top:0;max-width:46em;padding:5px 8px;border:1px solid var(--line);border-radius:6px;background:var(--panel);box-shadow:0 2px 10px rgb(0 0 0/.2);font-size:12px;line-height:1.45;white-space:pre-line;word-break:break-all;pointer-events:none}
#panel{position:fixed;z-index:9;top:32px;left:50%;transform:translateX(-50%);width:min(1000px,calc(100% - 24px));max-height:calc(100vh - 72px);overflow:auto;padding:12px 16px 16px;border:1px solid var(--line);border-radius:10px;background:var(--panel);box-shadow:0 8px 34px rgb(0 0 0/.25);cursor:auto;text-align:left}
#panel h2{font-size:10.5px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);margin:14px 0 6px}
#panel h2:first-of-type{margin-top:0}
#panel p{margin:6px 0;max-width:96ch}
#panel ul{margin:0;padding-left:16px}
#panel li{margin:2px 0;font-size:12px}
#close{position:absolute;top:9px;right:12px;cursor:pointer;color:var(--muted);background:none;border:0;font-size:13px;padding:0}
kbd{border:1px solid var(--line);border-bottom-width:2px;border-radius:4px;padding:0 4px;background:var(--bg)}
.keys{list-style:none;padding:0;display:grid;gap:3px;grid-template-columns:repeat(auto-fit,minmax(250px,1fr))}
.legend-list{list-style:none;margin:0;padding:0;display:grid;gap:5px;grid-template-columns:repeat(auto-fit,minmax(320px,1fr))}
.sw{display:inline-block;width:12px;height:12px;border-radius:3px;margin-right:6px;border:1px solid rgb(0 0 0/.3);vertical-align:-1px}
.sw.hatched{background-image:var(--hatch-gap)}
.sw.hatch{background:transparent;background-image:repeating-linear-gradient(45deg,var(--muted) 0 2px,transparent 2px 6px);border-color:var(--muted)}
table{border-collapse:collapse;width:100%;font-size:12px}
th,td{text-align:left;padding:4px 10px 4px 0;border-bottom:1px solid var(--line);vertical-align:top}
th{font-size:10.5px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted)}
.lv{display:inline-block;margin:0 8px 2px 0;white-space:nowrap}
`

// treeCSS is the tree view's own rules, kept out of styleSheet because a
// degenerate profile has no tree to press T for and should not carry 1.1 KB
// of rules for one. Same reasoning as reportCSS, which the graph page does
// not carry either.
//
// A row is a native <summary> inside a <details>, so the triangle is CSS on
// ::before rather than an aria-expanded the script has to keep in sync, and
// the marker the UA would draw is suppressed. The percentage and the value
// are fixed-width columns so the numbers line up down the page; the swatch
// takes its colour from the same [data-domain] rules the frames use, which
// is why those rules are not scoped to .frame.
const treeCSS = `
#tree{margin:8px 0 0;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}
#tree ul{list-style:none;margin:0;padding:0 0 0 15px}
#tree>ul{padding-left:0}
#tree li{white-space:nowrap}
#tree summary,#tree .leaf{display:block;padding:1px 3px;border-radius:3px;list-style:none}
#tree summary{cursor:pointer}
#tree .leaf{padding-left:16px}
#tree summary::-webkit-details-marker{display:none}
#tree summary::before{content:"\25B8";display:inline-block;width:13px;color:var(--muted)}
#tree details[open]>summary::before{content:"\25BE"}
#tree summary:hover,#tree .leaf:hover{background:var(--line)}
#tree summary:focus-visible{outline:2px solid var(--accent)}
#tree .p{display:inline-block;width:5em;text-align:right;color:var(--muted)}
#tree .v{display:inline-block;width:8.5em;text-align:right;color:var(--muted);padding-right:9px}
#tree .sw{width:10px;height:10px;margin-right:5px}
#tree .m{background:var(--fill-match);color:var(--frame-ink);border-radius:2px;padding:0 2px}
#tree .cur{outline:2px solid var(--accent);outline-offset:-2px}
`

// reportCSS styles the page a degenerate profile gets: no graph, no status
// bar, no info panel — a written report about why there is nothing to draw.
// It is emitted only on that page, so the 99% case does not carry it.
const reportCSS = `
body{padding:20px 16px 24px;max-width:1360px;margin:0 auto}
h1{font-size:19px;white-space:normal;letter-spacing:-.01em}
h2{font-size:11px;margin:0 0 8px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted)}
.sub{margin:2px 0 10px;color:var(--muted);font-size:12px;word-break:break-all}
.chips{display:flex;flex-wrap:wrap;gap:6px}
.chip{display:inline-flex;gap:6px;align-items:baseline;padding:3px 9px;border:1px solid var(--line);border-radius:999px;background:var(--panel);font-size:12px}
.chip b{font-weight:600;color:var(--muted);font-size:10.5px;text-transform:uppercase;letter-spacing:.06em}
section{margin:14px 0}
.notices,.nodata,.labels{border:1px solid var(--warn-line);border-radius:10px;padding:12px 14px;background:var(--warn-bg)}
.labels{background:var(--panel);border-color:var(--line)}
.notices.fatal,.nodata{background:var(--fatal-bg);border-color:var(--fatal-line)}
.notices ul{margin:0;padding-left:18px}
.nodata p{margin:0 0 8px;max-width:70ch}
footer{color:var(--muted);font-size:11.5px}
`

// The two knobs the dark theme is allowed to move, written once and emitted
// twice for the same reason as darkChrome. Nothing in either block is a
// colour: --hatch-ink aside, they are a lightness offset and a saturation
// multiplier that every fill already references.
const darkKnobs = `--fill-dl:-9%;--fill-ds:.9;--hatch-ink:rgb(22 20 18 / .20)`

// paletteCSS is the flame graph's own colours, kept apart from the page
// chrome above because it is emitted in two places: after styleSheet in the
// HTML page, and ahead of the graph when RenderFragment writes one for
// embedding. A graph pulled out of the page must still be the right colours.
//
// The palette is Brendan Gregg's AI Flame Graph palette — pink application,
// red/yellow/orange CPU, grey boundary, aqua accelerator source, green
// accelerator execution — designed for a white page. It has to survive a
// dark one too, and "survive" must not mean "become a different design", so
// no colour is defined twice. Every fill is one hsl() written once, at
// :root, with its hue fixed; the dark theme moves two numbers, --fill-dl and
// --fill-ds, which shift every fill's lightness and scale its saturation
// together. That is the whole of the dark theme's effect on the graph.
//
// Fills stay light in both themes, which is what makes a single near-black
// label colour legible everywhere; --frame-ink therefore has no dark
// variant, and neither do the hover and search outlines drawn on top of a
// fill. (var(--ink) would be wrong there: it inverts with the page, not with
// the frame.)
//
// Jitter. A frame does not take var(--fill-app) directly; it takes it through
// --fill-x and then adds a per-frame amount to that colour's LIGHTNESS,
// leaving its hue and saturation untouched. This is async-profiler's
// getColor() — a small random brightening of a base colour, per frame, so a
// run of same-domain neighbours has visible edges — with the randomness
// moved to render time and the arithmetic done in HSL.
//
// Lightening is not a preference, it is the only direction available. The
// darkest thing a label is drawn on clears the 4.5:1 contrast floor by 0.105
// (4.605:1, dark theme, hatched --fill-system), and raising a fill's
// lightness raises its luminance monotonically at fixed hue and saturation,
// so the worst-case jittered frame is the un-jittered one and the floor does
// not move. Darkening by as little as 1.5% would breach it.
//
// HSL rather than a white mix because this palette is already light: mixing
// 6% white into --fill-unsym moves it three units per channel and is
// invisible, while +6 lightness moves it fifteen and is not.
//
// The token is defined at :root but consumed on the frame, which is the only
// arrangement that works: var() inside a custom property is substituted when
// that property is computed, on the element that declares it, so a
// --fill-app that referenced --fill-j would freeze in :root's value of it.
// background-color: is a real property on the frame, so its var()s resolve
// there. The two hatch gradients are the opposite case and are fine: they
// reference --hatch-ink, which is declared on :root beside them, so freezing
// at :root is exactly what they want.
//
// The plain fill is declared first as a fallback. Relative colour syntax
// (hsl(from …)) is Chrome 119 / Safari 16.4 / Firefox 128; an engine that
// cannot parse the second declaration drops it and keeps the first, which is
// the palette's own colour. The failure mode of getting this wrong used to
// be a page of black rectangles (SVG's default fill); on a div it is a page
// of transparent ones, which is no better.
//
// Outlines are box-shadows rather than borders because a border participates
// in layout: with box-sizing:border-box it would eat 2 px out of a frame
// that may only be 3 px wide. An inset box-shadow is painted, not laid out.
// The one exception is the unattributed boundary, whose outline is dashed —
// box-shadow cannot dash — so it uses outline with a negative offset, which
// is also painted rather than laid out.
const paletteCSS = `
:root{
  --fill-dl:0%;
  --fill-ds:1;
  --fill-j:0;
  --hatch-ink:rgb(22 20 18 / .26);

  --hatch-gap: repeating-linear-gradient(45deg,var(--hatch-ink) 0 2.2px,transparent 2.2px 6px);
  --hatch-inf: repeating-linear-gradient(-45deg,var(--hatch-ink) 0 1.4px,transparent 1.4px 5px);

  --fill-app:      hsl(335 calc(78% * var(--fill-ds)) calc(79% + var(--fill-dl)));
  --fill-system:   hsl(  2 calc(68% * var(--fill-ds)) calc(78% + var(--fill-dl)));
  --fill-vendor:   hsl( 50 calc(88% * var(--fill-ds)) calc(69% + var(--fill-dl)));
  --fill-kernel:   hsl( 28 calc(92% * var(--fill-ds)) calc(70% + var(--fill-dl)));
  --fill-boundary: hsl(  0                        0%  calc(84% + var(--fill-dl)));
  --fill-boundary-unattributed: hsl(0             0%  calc(92% + var(--fill-dl)));
  --fill-gpu-kernel: hsl(128 calc(48% * var(--fill-ds)) calc(67% + var(--fill-dl)));
  --fill-accel-source: hsl(184 calc(52% * var(--fill-ds)) calc(65% + var(--fill-dl)));
  --fill-unsym: hsl( 45 calc(20% * var(--fill-ds)) calc(77% + var(--fill-dl)));
  --fill-shim:  hsl(215 calc(15% * var(--fill-ds)) calc(77% + var(--fill-dl)));
  --fill-root: hsl(30 calc(8% * var(--fill-ds)) calc(81% + var(--fill-dl)));

  --edge-frame:        rgb(255 255 255 / .55);
  --edge-boundary:     hsl(  0  0% calc(40% + var(--fill-dl)));
  --edge-unattributed: hsl(  0  0% calc(55% + var(--fill-dl)));
  --edge-shim:         hsl(215 24% calc(46% + var(--fill-dl)));

  --frame-ink: hsl(30 12% 9%);
  --fill-match: hsl(300 100% 71%);
}
@media (prefers-color-scheme: dark){:root:not([data-theme="light"]){` + darkKnobs + `}}
:root[data-theme="dark"]{` + darkKnobs + `}

.frame{color:var(--frame-ink);background-color:var(--fill-x);background-color:hsl(from var(--fill-x) h s calc(l + var(--fill-j)));box-shadow:inset 0 0 0 .5px var(--edge-frame)}
[data-domain="root"]{--fill-x:var(--fill-root)}
[data-domain="app"]{--fill-x:var(--fill-app)}
[data-domain="system"]{--fill-x:var(--fill-system)}
[data-domain="kernel"]{--fill-x:var(--fill-kernel)}
[data-domain="vendor"]{--fill-x:var(--fill-vendor)}
[data-domain="unsym"]{--fill-x:var(--fill-unsym);background-image:var(--hatch-gap)}
[data-domain="gpu-kernel"]{--fill-x:var(--fill-gpu-kernel)}
[data-domain="shim"]{--fill-x:var(--fill-shim);box-shadow:inset 0 0 0 1px var(--edge-shim)}
[data-domain="boundary"]{--fill-x:var(--fill-boundary);box-shadow:inset 0 0 0 1px var(--edge-boundary)}
[data-domain="boundary-unattributed"]{--fill-x:var(--fill-boundary-unattributed);background-image:var(--hatch-gap);box-shadow:none;outline:1px dashed var(--edge-unattributed);outline-offset:-1px}
.frame.inexact{background-image:var(--hatch-inf)}
.frame.inexact[data-domain="unsym"],.frame.inexact[data-domain="boundary-unattributed"]{background-image:var(--hatch-gap),var(--hatch-inf)}

.frame:hover{box-shadow:inset 0 0 0 1.4px var(--frame-ink);outline:none}
.frame.match{background-color:var(--fill-match)}
.frame.cur{box-shadow:inset 0 0 0 1.8px var(--frame-ink);outline:none}
.sw[data-domain]{background-color:var(--fill-x)}
`

// The jitter ladder. jitterSteps shades per domain, evenly spaced from 0 to
// jitterMaxL points of lightness. Step 0 is the palette's own colour and
// needs no class.
//
// 8 is the amplitude at which two shades three rungs apart (+3.4 lightness,
// about fifteen units per channel) are plainly different, while the highest
// rung of the palest jittered fill, --fill-unsym at 77%, only reaches 85% —
// still sand, not white. --fill-boundary-unattributed sits at 92% and would
// reach 100, which is one of the two reasons the boundary greys stay off the
// ladder; the other is that their lightness difference is a meaning (paler =
// nothing behind it), and a random lightness must not compete with a
// meaningful one.
const (
	jitterSteps = 8
	jitterMaxL  = 8.0
)

// jitterLightness is the lightness a ladder step adds, in HSL points.
func jitterLightness(step int) float64 {
	return float64(step) * jitterMaxL / float64(jitterSteps-1)
}

var jitterCSS = buildJitterCSS()

func buildJitterCSS() string {
	var b strings.Builder
	for i := 1; i < jitterSteps; i++ {
		fmt.Fprintf(&b, ".j%d{--fill-j:%.2f}\n", i, jitterLightness(i))
	}
	return b.String()
}

// rowCSS is the vertical half of the geometry: one rule per depth, and the
// two rules that turn a depth into an offset.
//
// A depth class carries a NUMBER, not an offset — .d3{--d:3} — and the frame
// multiplies it by the row pitch. That indirection is what buys the icicle:
// the flame graph offsets from the bottom because it grows upward from its
// root, the icicle offsets from the top because it grows downward from it,
// and the difference is one extra rule rather than a second ladder of
// per-depth offsets. It is also shorter than the offsets were — ".d59{--d:59}"
// against ".d59{bottom:1062px}" — and a profile has far more frames than
// depths, which is why the depth is a class at all and not an inline style.
//
// Both rules are in px and neither mentions the window, so the row pitch is
// the same number of pixels at 800px and at 1990px, in either direction.
func rowCSS(maxDepth int) string {
	var b strings.Builder
	fmt.Fprintf(&b, ".frame{bottom:calc(var(--d)*%dpx)}\n.inv .frame{bottom:auto;top:calc(var(--d)*%dpx)}\n", frameHeight, frameHeight)
	for d := range maxDepth {
		fmt.Fprintf(&b, ".d%d{--d:%d}", d, d)
	}
	b.WriteByte('\n')
	return b.String()
}

// script is the whole of the page's interactivity. Three things in it are
// not obvious:
//
//   - Frame geometry is read back off the elements' own inline styles at
//     start-up rather than duplicated into data-orig-* attributes. A frame
//     has to carry left and width anyway to draw with no script at all, and
//     a second copy of every coordinate cost 916 bytes on a 20-frame profile
//     and scales with frame count.
//   - Both are percentages, so zoom is percentage arithmetic and there is no
//     resize handler: a zoomed graph is still expressed relative to .chart,
//     and .chart is still whatever the window is. The layout survives a
//     resize because nothing in it was ever in pixels.
//   - fmt() mirrors flamegraph.FormatValue exactly: never invent a unit the
//     profile did not give, and never call nanoseconds "samples".
//
// The three views it adds to that:
//
// toggleInvert is async-profiler's I, and theirs is a visual flip: the same
// tree, drawn from the top down. It merges nothing and aggregates nothing —
// the callee-centric reverse merge is their converter's --reverse flag,
// which builds a different tree before the page exists. So inverting here is
// one class on the chart, which swaps every frame's offset from bottom to
// top (see rowCSS); no width, no value and no zoom is touched, and inverting
// twice is the page it started as.
//
// buildTree reconstructs the call tree from the frames already in the
// document. A frame carries its depth as a class and writeNode emits frames
// in pre-order, so depth plus document order is a parent link — which means
// the tree view costs no markup at all until someone presses T, and no
// symbol is ever on the page twice. Rows are made when their parent first
// opens, so a 20,000-frame profile builds the rows someone looked at; a
// frame with exactly one child opens all the way through, because this
// profile's CPU path is a corridor fourteen frames long with no doors off
// it and clicking fourteen times down it is not navigation.
//
// applySearch counts over the whole profile, not over what the zoom happens
// to show: a count and a share that changed when you zoomed would answer a
// different question from the one the reader asked. nav holds only the
// OUTERMOST matches — a match inside a match is highlighted but not walked
// to, and its value is not added a second time, so the share can never
// exceed 100%. Stepping (N, Shift+N, and Enter from inside the field, which
// swallows N) zooms the flame view to the match or reveals it in the tree.
// A match outside the tree's current root has no row to scroll to, so the
// tree drops back to the whole profile rather than doing nothing.
const script = `
(function(){
"use strict";
var doc=document,root=doc.documentElement;
var chart=doc.querySelector(".chart");
if(!chart){return;}
var info=doc.getElementById("info");
var st=doc.getElementById("st"),mc=doc.getElementById("mc");
var q=doc.getElementById("q"),tip=doc.getElementById("tip");
var tree=doc.getElementById("tree");
var unit=chart.dataset.unit||"",total=+chart.dataset.total||0;
var mods=(function(){var e=doc.getElementById("modules");if(!e){return [];}
 try{return JSON.parse(e.textContent)||[];}catch(_){return [];}})();
function moduleOf(d){var i=d.m;return i===undefined?"":(mods[+i]||"");}
var idle=st.textContent,zoomTarget=null;
var axis=chart.getAttribute("aria-label");
var inverted=false,treeOn=false,treeRoot=null;
var nav=[],navIdx=-1,cur=null,tally="";

var items=[].map.call(chart.querySelectorAll(".frame"),function(el){
  var name=el.textContent;
  return {el:el,name:name,lower:name.toLowerCase(),
    d:+/(?:^| )d(\d+)(?: |$)/.exec(el.className)[1],
    value:+el.dataset.value||0,inexact:+el.dataset.inexact||0,
    x:parseFloat(el.style.left),w:parseFloat(el.style.width),shown:true,kids:[]};
});
(function(){
  var stack=[];
  items.forEach(function(it){
    stack.length=it.d;
    if(it.d>0){it.up=stack[it.d-1];it.up.kids.push(it);}
    stack.push(it);
  });
  items.forEach(function(it){
    it.kids.sort(function(a,b){return b.value-a.value;});
    it.self=it.value;
    it.kids.forEach(function(c){it.self-=c.value;});
  });
})();

function grouped(n){
  var s=String(Math.round(n)),out="",i;
  for(i=0;i<s.length;i++){
    if(i>0&&(s.length-i)%3===0){out+=",";}
    out+=s.charAt(i);
  }
  return out;
}
function fmt(v){
  if(unit==="nanoseconds"){
    if(v===0){return "0 ns";}
    if(v<1e3){return grouped(v)+" ns";}
    if(v<1e6){return (v/1e3).toFixed(2)+" µs";}
    if(v<1e9){return (v/1e6).toFixed(2)+" ms";}
    return (v/1e9).toFixed(3)+" s";
  }
  if(unit===""){return grouped(v);}
  return grouped(v)+" "+unit;
}
function pct(v){return total?(v/total*100).toFixed(2)+"%":"0%";}
function place(it,x,w,vis){
  it.shown=vis;
  it.el.style.display=vis?"":"none";
  if(!vis){return;}
  it.el.style.left=x+"%";
  it.el.style.width=Math.max(0,w)+"%";
}
function line(it){return it.name+"  ·  "+fmt(it.value)+"  ·  "+pct(it.value)+" of total";}
function detail(it){
  var s=line(it),d=it.el.dataset,m=moduleOf(d);
  if(it.kids.length&&it.self>0){s+="\nself: "+fmt(it.self);}
  if(m){s+="\nmodule: "+m;}
  var w=widthMeaning(it,d);
  if(w){s+="\n"+w;}
  if(it.inexact>0){s+="\n"+fmt(it.inexact)+" of this is attributed by inference, not measurement";}
  if(d.domain==="unsym"){s+="\nno symbol: the unwind found this frame, nothing could name it";}
  return s;
}
// What this frame's width actually measures.
//
// Only said where it is NOT the obvious thing, so it stays a warning rather
// than a caption. In a GPU profile every CPU frame is as wide as the device
// time launched from it, not the CPU time spent in it -- a reader who knows
// flame graphs will assume the opposite, and the profile carries no other
// signal that would correct them. That semantic was documented only in prose
// on a page most viewers never open (issue #123).
function widthMeaning(it,d){
  if(unit.indexOf("nanoseconds")<0||axis.indexOf("gpu/")<0){return "";}
  switch(d.domain){
  case "gpu-kernel": return "width: measured time this kernel ran on the device";
  case "boundary": return "width: device time launched from this path, sampled one launch in N";
  case "boundary-unattributed": return "width: measured device time whose launch was not stack-sampled, so it has no caller";
  default: return "width: GPU time launched from this call path \u2014 not CPU time spent here";
  }
}
function status(it){st.textContent=it?line(it):(zoomTarget?line(zoomTarget):idle);}
function showTip(evt,it){
  tip.textContent=detail(it);
  tip.style.left=tip.style.top="0px";
  tip.hidden=false;
  var box=tip.getBoundingClientRect(),pad=14;
  var x=evt.clientX+pad,y=evt.clientY+pad;
  if(x+box.width>innerWidth-6){x=Math.max(6,evt.clientX-pad-box.width);}
  if(y+box.height>innerHeight-30){y=Math.max(6,evt.clientY-pad-box.height);}
  tip.style.left=x+"px";
  tip.style.top=y+"px";
}
function reset(){
  zoomTarget=null;
  items.forEach(function(it){place(it,it.x,it.w,true);});
  status(null);
}
function zoom(target){
  if(!(target.w>0)){return;}
  var tEnd=target.x+target.w,ratio=100/target.w;
  zoomTarget=target;
  items.forEach(function(it){
    var end=it.x+it.w;
    if(it.x<=target.x&&end>=tEnd){place(it,0,100,true);return;}
    if(it.x>=target.x&&end<=tEnd){place(it,(it.x-target.x)*ratio,it.w*ratio,true);return;}
    place(it,it.x,it.w,false);
  });
  status(null);
}

function applySearch(){
  var s=q.value.trim().toLowerCase(),n=0,v=0,end=-1,keep=cur;
  nav=[];
  items.forEach(function(it){
    var hit=s!==""&&it.lower.indexOf(s)!==-1;
    it.hit=hit;
    it.el.classList.toggle("match",hit);
    it.el.classList.toggle("dim",s!==""&&!hit);
    it.el.classList.remove("cur");
    if(it.nm){it.nm.classList.toggle("m",hit);}
    if(it.head){it.head.classList.remove("cur");}
    if(hit){
      n++;
      if(it.x>=end){nav.push(it);v+=it.value;end=it.x+it.w;}
    }
  });
  tally=s===""?"":(n===0?"no match":grouped(n)+" matched · "+fmt(v)+" · "+pct(v));
  navIdx=keep?nav.indexOf(keep):-1;
  cur=navIdx<0?null:keep;
  if(cur){markCur();}
  showTally();
}
function showTally(){
  mc.textContent=tally+(cur?" ("+(navIdx+1)+" of "+nav.length+")":"");
}
function markCur(){
  cur.el.classList.add("cur");
  if(cur.head){cur.head.classList.add("cur");}
}
function stepMatch(dir){
  if(nav.length===0){return;}
  if(cur){cur.el.classList.remove("cur");if(cur.head){cur.head.classList.remove("cur");}}
  navIdx=(navIdx+dir+nav.length)%nav.length;
  cur=nav[navIdx];
  if(treeOn){revealInTree(cur);}else{zoom(cur);cur.el.scrollIntoView({block:"center"});}
  markCur();
  status(cur);
  showTally();
}
function openSearch(){q.hidden=false;q.focus();q.select();}
function closeSearch(){q.value="";cur=null;applySearch();q.hidden=true;q.blur();}

function toggleInfo(){info.open=!info.open;}
function toggleTheme(){
  var dark=root.dataset.theme?root.dataset.theme==="dark":matchMedia("(prefers-color-scheme: dark)").matches;
  root.dataset.theme=dark?"light":"dark";
}

function toggleInvert(){
  inverted=!inverted;
  chart.classList.toggle("inv",inverted);
  chart.setAttribute("aria-label",inverted?axis+", inverted: root at top":axis);
  tip.hidden=true;
}

function toggleTree(){
  treeOn=!treeOn;
  if(treeOn){buildTree();}
  tip.hidden=true;
  tree.hidden=!treeOn;
  chart.hidden=treeOn;
}
function buildTree(){
  var r=zoomTarget||items[0];
  if(treeRoot===r){return;}
  treeRoot=r;
  items.forEach(function(it){it.det=it.head=it.nm=it.filled=null;});
  tree.textContent="";
  var ul=doc.createElement("ul");
  ul.appendChild(makeRow(r));
  tree.appendChild(ul);
  expand(r);
}
function makeRow(it){
  var li=doc.createElement("li"),head;
  if(it.kids.length){
    var det=doc.createElement("details");
    head=doc.createElement("summary");
    det.appendChild(head);
    det.addEventListener("toggle",function(){if(det.open){expand(it);}});
    li.appendChild(det);
    it.det=det;
  }else{
    head=doc.createElement("div");
    head.className="leaf";
    li.appendChild(head);
  }
  var sw=doc.createElement("span");
  sw.className="sw";
  sw.dataset.domain=it.el.dataset.domain||"";
  sw.setAttribute("aria-hidden","true");
  var p=doc.createElement("span");
  p.className="p";
  p.textContent=pct(it.value);
  var v=doc.createElement("span");
  v.className="v";
  v.textContent=fmt(it.value);
  var nm=doc.createElement("span");
  nm.textContent=it.name;
  if(it.hit){nm.className="m";}
  head.appendChild(p);
  head.appendChild(v);
  head.appendChild(sw);
  head.appendChild(nm);
  head.title=detail(it);
  it.head=head;
  it.nm=nm;
  if(cur===it){head.classList.add("cur");}
  return li;
}
function expand(it){
  if(!it.kids.length){return;}
  if(!it.filled){
    it.filled=true;
    var ul=doc.createElement("ul");
    it.kids.forEach(function(c){ul.appendChild(makeRow(c));});
    it.det.appendChild(ul);
  }
  it.det.open=true;
  if(it.kids.length===1){expand(it.kids[0]);}
}
function revealInTree(it){
  var chain=[],p=it;
  while(p&&p!==treeRoot){chain.push(p);p=p.up;}
  if(p!==treeRoot){
    treeRoot=null;
    zoomTarget=null;
    buildTree();
    return revealInTree(it);
  }
  chain.push(treeRoot);
  chain.reverse();
  chain.forEach(function(o,i){if(i<chain.length-1){expand(o);}});
  if(it.head){it.head.scrollIntoView({block:"center"});}
}

items.forEach(function(it){
  it.el.addEventListener("click",function(e){e.stopPropagation();zoom(it);});
  it.el.addEventListener("mouseenter",function(){status(it);});
  it.el.addEventListener("mousemove",function(e){showTip(e,it);});
  it.el.addEventListener("mouseleave",function(){tip.hidden=true;status(null);});
});
chart.addEventListener("click",function(e){if(e.target===chart){reset();}});
q.addEventListener("input",function(){cur=null;applySearch();});
doc.getElementById("theme-btn").addEventListener("click",toggleTheme);
doc.getElementById("inv-btn").addEventListener("click",toggleInvert);
doc.getElementById("tree-btn").addEventListener("click",toggleTree);
doc.getElementById("close").addEventListener("click",function(){info.open=false;});
doc.addEventListener("click",function(e){
  if(info.open&&!info.contains(e.target)){info.open=false;}
});
doc.addEventListener("keydown",function(e){
  if((e.ctrlKey||e.metaKey)&&(e.key==="f"||e.key==="F")){e.preventDefault();openSearch();return;}
  if(e.key==="Escape"){
    if(info.open){info.open=false;}
    else if(!q.hidden){closeSearch();}
    else{reset();}
    return;
  }
  if(doc.activeElement===q){
    if(e.key==="Enter"){e.preventDefault();stepMatch(e.shiftKey?-1:1);}
    return;
  }
  if(e.ctrlKey||e.metaKey||e.altKey){return;}
  if(e.key==="0"){reset();}
  else if(e.key==="d"||e.key==="D"){toggleTheme();}
  else if(e.key==="i"||e.key==="I"){toggleInvert();}
  else if(e.key==="t"||e.key==="T"){toggleTree();}
  else if(e.key==="n"){stepMatch(1);}
  else if(e.key==="N"){stepMatch(-1);}
  else if(e.key==="?"){toggleInfo();}
});
reset();
})();
`
