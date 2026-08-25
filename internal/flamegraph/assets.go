package flamegraph

// Everything in this file is inlined verbatim into the output page. Nothing
// here may reference an external URL: the deliverable is one file that
// renders correctly from file:// with no server and no network.
//
// No user- or symbol-derived text is ever placed inside <style> or <script>.
// All profile data reaches the page through escaped SVG attributes and is
// read back with dataset/textContent, so a mangled C++ name full of <, > and
// & cannot inject markup or script.

// svgDefs holds the hatch patterns. Hatching always means "the profile could
// not fully account for this frame" — no symbol, or no measured caller.
const svgDefs = `<defs>
<pattern id="p-gap" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
<rect width="6" height="6" fill="none"/>
<line class="hatch" x1="0" y1="0" x2="0" y2="6" stroke-width="2.2"/>
</pattern>
<pattern id="p-inferred" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(-45)">
<rect width="5" height="5" fill="none"/>
<line class="hatch" x1="0" y1="0" x2="0" y2="5" stroke-width="1.6" stroke-dasharray="2 2"/>
</pattern>
</defs>
`

const toolbarHTML = `<section class="toolbar">
<div class="row">
<label class="search"><span>Search</span><input id="search" type="search" placeholder="frame name or substring" autocomplete="off" spellcheck="false"></label>
<button id="clear-search" type="button">Clear</button>
<button id="reset-zoom" type="button">Reset zoom</button>
<span class="spacer"></span>
<span class="stat"><b>matched</b><span id="match-count">&mdash;</span></span>
<span class="stat"><b>focus</b><span id="zoom-state">all</span></span>
</div>
<div class="crumbs" id="breadcrumbs">all</div>
<div class="detail" id="details">Hover a frame for its value. Click to zoom. Press / to search, Esc to clear then reset.</div>
</section>
`

const styleSheet = `
:root{
  --bg:#fbf9f6; --panel:#fffefc; --ink:#1d1a17; --muted:#6b6259;
  --line:#e2dad0; --accent:#b4522a; --warn-bg:#fff6e8; --warn-line:#e8c88a;
  --fatal-bg:#fdecec; --fatal-line:#e0a3a3;
}
@media (prefers-color-scheme: dark){
  :root{
    --bg:#16151a; --panel:#1e1d23; --ink:#eceaf2; --muted:#a29caf;
    --line:#332f3b; --accent:#ff9a6a; --warn-bg:#2b2313; --warn-line:#6d5722;
    --fatal-bg:#2e1616; --fatal-line:#7a3232;
  }
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);
  font:14px/1.5 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif}
main{max-width:1360px;margin:0 auto;padding:20px 16px 48px}
h1{font-size:20px;margin:0 0 2px;letter-spacing:-.01em}
h2{font-size:13px;margin:0 0 8px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted)}
code{font:12px/1.4 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.sub{margin:0 0 10px;color:var(--muted);font-size:12.5px;word-break:break-all}
.muted{color:var(--muted)}
.hdr{margin-bottom:14px}
.chips{display:flex;flex-wrap:wrap;gap:6px}
.chip{display:inline-flex;gap:6px;align-items:baseline;padding:3px 9px;border:1px solid var(--line);
  border-radius:999px;background:var(--panel);font-size:12px}
.chip b{font-weight:600;color:var(--muted);font-size:10.5px;text-transform:uppercase;letter-spacing:.06em}
section{margin:0 0 14px}
.notices,.nodata,.legend,.labels{border:1px solid var(--line);border-radius:10px;background:var(--panel);padding:12px 14px}
.notices{background:var(--warn-bg);border-color:var(--warn-line)}
.notices.fatal,.nodata{background:var(--fatal-bg);border-color:var(--fatal-line)}
.notices ul{margin:0;padding-left:18px}
.notices li{margin:3px 0;font-size:13px}
.nodata h2{color:inherit}
.nodata p{margin:0 0 8px;max-width:70ch}
.toolbar{position:sticky;top:0;z-index:5;border:1px solid var(--line);border-radius:10px;
  background:var(--panel);padding:10px 12px}
.row{display:flex;flex-wrap:wrap;gap:8px;align-items:center}
.search{display:inline-flex;gap:6px;align-items:center}
.search span{font-size:11px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted)}
input,button{font:inherit;color:inherit;background:var(--bg);border:1px solid var(--line);
  border-radius:7px;padding:5px 9px}
input{min-width:260px}
button{cursor:pointer}
button:hover{border-color:var(--accent)}
.spacer{flex:1}
.stat{display:inline-flex;gap:6px;align-items:baseline;font-size:12px}
.stat b{font-size:10.5px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted)}
.crumbs{margin-top:8px;font-size:12px;color:var(--muted);word-break:break-word;min-height:1.4em}
.detail{margin-top:4px;font-size:12.5px;min-height:1.4em}
.chart{margin:12px 0;overflow-x:auto;border:1px solid var(--line);border-radius:10px;background:var(--panel)}
svg.flame{display:block}
svg.flame text{font:11px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;pointer-events:none}
g.frame{cursor:pointer}
g.frame.dim{opacity:.15}
.legend-list{list-style:none;margin:0;padding:0;display:grid;gap:5px;
  grid-template-columns:repeat(auto-fit,minmax(330px,1fr))}
.legend-list li{font-size:12.5px;line-height:1.45}
.legend-list b{margin-right:4px}
.sw{display:inline-block;width:14px;height:14px;border-radius:3px;margin-right:7px;
  border:1px solid rgba(0,0,0,.3);vertical-align:-2px}
.sw.hatched{background-image:repeating-linear-gradient(45deg,var(--hatch-ink) 0 2px,transparent 2px 6px)}
/* The generic hatching row explains the pattern, not a domain, so its
   swatch carries the pattern over nothing. */
.sw.hatch{background:transparent;
  background-image:repeating-linear-gradient(45deg,var(--muted) 0 2px,transparent 2px 6px);
  border-color:var(--muted)}
.legend-note{margin:10px 0 0;max-width:90ch;font-size:12.5px}
.depth-note{margin:10px 0 0;max-width:90ch;font-size:12.5px}
.labels table{border-collapse:collapse;width:100%;font-size:12.5px}
.labels th,.labels td{text-align:left;padding:5px 10px 5px 0;border-bottom:1px solid var(--line);
  vertical-align:top}
.labels th{font-size:10.5px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted)}
.lv{display:inline-block;margin:0 8px 2px 0;white-space:nowrap}
footer{color:var(--muted);font-size:11.5px;padding-top:6px}
`

// paletteCSS is the flame graph's own colours, kept apart from the page
// chrome above because it is emitted in two places: after styleSheet in the
// HTML page, and inside the <svg> itself when RenderSVG writes a standalone
// graph. A graph embedded on its own must still be the right colours.
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
const paletteCSS = `
:root{
  /* The two theme knobs. Nothing else about the palette changes. */
  --fill-dl:0%;   /* lightness shift applied to every fill and every edge */
  --fill-ds:1;    /* saturation multiplier applied to every fill */
  --hatch-ink:rgb(22 20 18 / .26);

  /* Gregg's layers. Hue is the meaning; it never moves. */
  --fill-app:      hsl(335 calc(78% * var(--fill-ds)) calc(79% + var(--fill-dl)));
  --fill-system:   hsl(  2 calc(68% * var(--fill-ds)) calc(78% + var(--fill-dl)));
  --fill-vendor:   hsl( 50 calc(88% * var(--fill-ds)) calc(69% + var(--fill-dl)));
  --fill-kernel:   hsl( 28 calc(92% * var(--fill-ds)) calc(70% + var(--fill-dl)));
  --fill-boundary: hsl(  0                        0%  calc(84% + var(--fill-dl)));
  --fill-boundary-unattributed: hsl(0             0%  calc(92% + var(--fill-dl)));
  --fill-gpu-kernel: hsl(128 calc(48% * var(--fill-ds)) calc(67% + var(--fill-dl)));

  /* Reserved. Gregg's aqua is the source of functions running ON the
     accelerator; that needs GPU PC sampling with SASS→source resolution,
     which perf-agent does not do yet. No domain points at this token, so
     nothing on the page is aqua — the hue is held open, not spent. */
  --fill-accel-source: hsl(184 calc(52% * var(--fill-ds)) calc(65% + var(--fill-dl)));

  /* The two domains Gregg's scheme has no layer for. Drained to near-grey
     rather than given a sixth hue: warm for "a CPU frame we cannot name",
     cool for "not your program at all". */
  --fill-unsym: hsl( 45 calc(20% * var(--fill-ds)) calc(77% + var(--fill-dl)));
  --fill-shim:  hsl(215 calc(15% * var(--fill-ds)) calc(77% + var(--fill-dl)));

  --fill-root: hsl(30 calc(8% * var(--fill-ds)) calc(81% + var(--fill-dl)));

  /* Edges shift with the fills so each keeps its contrast against its own
     frame rather than against the page. */
  --edge-frame:        rgb(255 255 255 / .55);
  --edge-boundary:     hsl(  0  0% calc(40% + var(--fill-dl)));
  --edge-unattributed: hsl(  0  0% calc(55% + var(--fill-dl)));
  --edge-shim:         hsl(215 24% calc(46% + var(--fill-dl)));

  /* Ink on a fill. Fills are light in both themes, so this is too. */
  --frame-ink: hsl(30 12% 9%);
}
@media (prefers-color-scheme: dark){
  :root{ --fill-dl:-9%; --fill-ds:.9; --hatch-ink:rgb(22 20 18 / .20); }
}

svg.flame text{fill:var(--frame-ink)}
pattern line.hatch{stroke:var(--hatch-ink)}

g.frame rect.bg{stroke:var(--edge-frame);stroke-width:.5}
g.frame[data-domain="root"] rect.bg{fill:var(--fill-root)}
g.frame[data-domain="app"] rect.bg{fill:var(--fill-app)}
g.frame[data-domain="system"] rect.bg{fill:var(--fill-system)}
g.frame[data-domain="kernel"] rect.bg{fill:var(--fill-kernel)}
g.frame[data-domain="vendor"] rect.bg{fill:var(--fill-vendor)}
g.frame[data-domain="unsym"] rect.bg{fill:var(--fill-unsym)}
g.frame[data-domain="gpu-kernel"] rect.bg{fill:var(--fill-gpu-kernel)}
/* The three domains that are not the program's own computation get an
   outline as well as a fill, so they read as themselves at one pixel wide.
   The dashed one is [gpu:launch unsampled]: a boundary with no CPU stack
   behind it, which must never be mistaken for [gpu:launch], which has one. */
g.frame[data-domain="shim"] rect.bg{fill:var(--fill-shim);stroke:var(--edge-shim);stroke-width:1}
g.frame[data-domain="boundary"] rect.bg{fill:var(--fill-boundary);stroke:var(--edge-boundary);stroke-width:1}
g.frame[data-domain="boundary-unattributed"] rect.bg{fill:var(--fill-boundary-unattributed);stroke:var(--edge-unattributed);stroke-width:1;stroke-dasharray:3 2}

/* Last, so hover and search win over any domain outline above. */
g.frame:hover rect.bg{stroke:var(--frame-ink);stroke-width:1.4;stroke-dasharray:none}
g.frame.match rect.bg{stroke:var(--frame-ink);stroke-width:1.8;stroke-dasharray:none}
`

const script = `
(function(){
"use strict";
var svg=document.querySelector("svg.flame");
if(!svg){return;}
var frames=Array.prototype.slice.call(svg.querySelectorAll("g.frame"));
var sidePad=parseFloat(svg.dataset.sidePad)||0;
var plotWidth=parseFloat(svg.dataset.plotWidth)||1;
var minText=parseFloat(svg.dataset.minText)||24;
var textPad=parseFloat(svg.dataset.textPad)||4;
var charWidth=parseFloat(svg.dataset.charWidth)||6.4;
var unit=svg.dataset.unit||"";
var total=Number(svg.dataset.total||"0");

var details=document.getElementById("details");
var crumbs=document.getElementById("breadcrumbs");
var matchCount=document.getElementById("match-count");
var zoomState=document.getElementById("zoom-state");
var search=document.getElementById("search");
var clearBtn=document.getElementById("clear-search");
var resetBtn=document.getElementById("reset-zoom");
var zoomTarget=null;
var hintText=details.textContent;

function num(el,key){return Number(el.dataset[key]||"0");}

function grouped(n){
  var s=String(Math.round(n)),out="",i;
  for(i=0;i<s.length;i++){
    if(i>0&&(s.length-i)%3===0){out+=",";}
    out+=s.charAt(i);
  }
  return out;
}
/* Mirrors flamegraph.FormatValue: never invent a unit the profile did not
   give, and never call nanoseconds "samples". */
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
function pct(v){
  if(!total){return "0%";}
  return (v/total*100).toFixed(2)+"%";
}
function fit(name,width){
  var max=Math.floor((width-textPad*2)/charWidth);
  if(max<=0){return "";}
  var chars=Array.from(name);
  if(chars.length<=max){return name;}
  if(max<=2){return chars.slice(0,max).join("");}
  return chars.slice(0,max-1).join("")+"…";
}
function place(el,x,width,visible){
  el.style.display=visible?"":"none";
  if(!visible){return;}
  var rects=el.querySelectorAll("rect"),i;
  for(i=0;i<rects.length;i++){
    rects[i].setAttribute("x",x.toFixed(3));
    rects[i].setAttribute("width",Math.max(0,width).toFixed(3));
  }
  var text=el.querySelector("text");
  if(text){
    if(width>=minText){
      text.style.display="";
      text.setAttribute("x",(x+textPad).toFixed(3));
      text.textContent=fit(el.dataset.name,width);
    }else{
      text.style.display="none";
    }
  }
}
function describe(el){
  var v=num(el,"value"),inexact=num(el,"inexact");
  var s=el.dataset.name+" — "+fmt(v)+" ("+pct(v)+" of total) — "+el.dataset.domainLabel;
  if(inexact>0){s+=" — "+fmt(inexact)+" of it attributed by inference, not measurement";}
  return s;
}
function showPath(el){
  crumbs.textContent=el?el.dataset.path:"all";
}
function select(el){
  if(!el){details.textContent=hintText;showPath(zoomTarget);return;}
  details.textContent=describe(el);
  showPath(el);
}
function reset(){
  zoomTarget=null;
  zoomState.textContent="all";
  frames.forEach(function(el){place(el,num(el,"origX"),num(el,"origWidth"),true);});
  select(null);
  applySearch(search.value);
}
function zoom(target){
  var tx=num(target,"origX"),tw=num(target,"origWidth");
  if(!(tw>0)){return;}
  var tEnd=tx+tw,ratio=plotWidth/tw;
  zoomTarget=target;
  zoomState.textContent=target.dataset.name;
  frames.forEach(function(el){
    var x=num(el,"origX"),w=num(el,"origWidth"),end=x+w;
    var ancestor=x<=tx&&end>=tEnd;
    var descendant=x>=tx&&end<=tEnd;
    if(ancestor){place(el,sidePad,plotWidth,true);return;}
    if(descendant){place(el,sidePad+(x-tx)*ratio,w*ratio,true);return;}
    place(el,x,w,false);
  });
  select(target);
  applySearch(search.value);
}
function applySearch(query){
  var q=(query||"").trim().toLowerCase();
  var matched=0,matchedValue=0;
  frames.forEach(function(el){
    var visible=el.style.display!=="none";
    var hit=q!==""&&el.dataset.name.toLowerCase().indexOf(q)!==-1;
    el.classList.toggle("match",hit&&visible);
    el.classList.toggle("dim",q!==""&&visible&&!hit);
    if(hit&&visible){matched++;matchedValue+=num(el,"value");}
  });
  if(q===""){matchCount.textContent="—";return;}
  matchCount.textContent=grouped(matched)+" frames, "+fmt(matchedValue);
}

frames.forEach(function(el){
  el.addEventListener("click",function(evt){evt.stopPropagation();zoom(el);});
  el.addEventListener("mouseenter",function(){select(el);});
  el.addEventListener("mouseleave",function(){select(zoomTarget);});
});
svg.addEventListener("click",function(evt){if(evt.target===svg){reset();}});
search.addEventListener("input",function(e){applySearch(e.target.value);});
clearBtn.addEventListener("click",function(){search.value="";applySearch("");search.focus();});
resetBtn.addEventListener("click",reset);
document.addEventListener("keydown",function(evt){
  if(evt.key==="/"&&document.activeElement!==search){
    evt.preventDefault();search.focus();search.select();return;
  }
  if(evt.key==="Escape"){
    if(search.value!==""){search.value="";applySearch("");}
    else if(zoomTarget){reset();}
  }
});
reset();
})();
`
