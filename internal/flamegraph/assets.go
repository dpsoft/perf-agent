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
<line x1="0" y1="0" x2="0" y2="6" stroke="rgba(20,20,20,.30)" stroke-width="2.2"/>
</pattern>
<pattern id="p-inferred" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(-45)">
<rect width="5" height="5" fill="none"/>
<line x1="0" y1="0" x2="0" y2="5" stroke="rgba(10,10,10,.34)" stroke-width="1.6" stroke-dasharray="2 2"/>
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
svg.flame text{font:11px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;fill:#14120f;pointer-events:none}
@media (prefers-color-scheme: dark){svg.flame text{fill:#100f14}}
g.frame{cursor:pointer}
g.frame rect.bg{stroke:rgba(255,255,255,.55);stroke-width:.5}
g.frame:hover rect.bg{stroke:var(--ink);stroke-width:1.4}
g.frame.match rect.bg{stroke:#111;stroke-width:1.6}
g.frame.dim{opacity:.15}
.legend-list{list-style:none;margin:0;padding:0;display:grid;gap:5px;
  grid-template-columns:repeat(auto-fit,minmax(330px,1fr))}
.legend-list li{font-size:12.5px;line-height:1.45}
.legend-list b{margin-right:4px}
.sw{display:inline-block;width:14px;height:14px;border-radius:3px;margin-right:7px;
  border:1px solid rgba(0,0,0,.3);vertical-align:-2px}
.sw.hatched{background-image:repeating-linear-gradient(45deg,rgba(20,20,20,.34) 0 2px,transparent 2px 6px)}
.sw.hatch{background:#c9c9c9;
  background-image:repeating-linear-gradient(45deg,rgba(20,20,20,.45) 0 2px,transparent 2px 6px);
  border-color:var(--muted)}
.depth-note{margin:10px 0 0;max-width:90ch;font-size:12.5px}
.labels table{border-collapse:collapse;width:100%;font-size:12.5px}
.labels th,.labels td{text-align:left;padding:5px 10px 5px 0;border-bottom:1px solid var(--line);
  vertical-align:top}
.labels th{font-size:10.5px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted)}
.lv{display:inline-block;margin:0 8px 2px 0;white-space:nowrap}
footer{color:var(--muted);font-size:11.5px;padding-top:6px}
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
