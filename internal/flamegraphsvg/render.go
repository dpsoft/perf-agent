package flamegraphsvg

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"math"
	"strconv"
	"strings"
)

const (
	defaultWidth      = 1200
	defaultFrameH     = 18
	defaultFontSize   = 12
	defaultTopPad     = 32
	defaultBottomPad  = 24
	defaultSidePad    = 10
	defaultTextPad    = 3
	defaultMinTextW   = 20
	defaultBackground = "#f8f4f0"
)

type Options struct {
	Title string
	Width int
}

type node struct {
	name     string
	value    uint64
	children []*node
	index    map[string]*node
}

func Render(w io.Writer, folded io.Reader, opts Options) error {
	root, maxDepth, err := parseFolded(folded)
	if err != nil {
		return err
	}
	return renderParsed(w, root, maxDepth, opts)
}

func renderParsed(w io.Writer, root *node, maxDepth int, opts Options) error {
	if opts.Width <= 0 {
		opts.Width = defaultWidth
	}
	if opts.Title == "" {
		opts.Title = "Flame Graph"
	}
	if root.value == 0 {
		return fmt.Errorf("no folded samples")
	}

	canvasHeight := defaultTopPad + defaultBottomPad + maxDepth*defaultFrameH
	plotWidth := float64(opts.Width - defaultSidePad*2)
	scale := plotWidth / float64(root.value)

	if _, err := fmt.Fprintf(w, `<svg version="1.1" width="%d" height="%d" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" data-total="%d" data-max-depth="%d">`+"\n", opts.Width, canvasHeight, opts.Width, canvasHeight, root.value, maxDepth); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, `<rect x="0" y="0" width="%d" height="%d" fill="%s" />`+"\n", opts.Width, canvasHeight, defaultBackground); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, `<text x="%d" y="20" font-size="16" font-family="Verdana" fill="#222">%s</text>`+"\n", defaultSidePad, html.EscapeString(opts.Title)); err != nil {
		return err
	}

	x := float64(defaultSidePad)
	for _, child := range root.children {
		x = renderNode(w, child, x, 0, maxDepth, scale, "")
	}

	_, err := io.WriteString(w, "</svg>\n")
	return err
}

func RenderHTML(w io.Writer, folded io.Reader, opts Options) error {
	root, maxDepth, err := parseFolded(folded)
	if err != nil {
		return err
	}
	var svg strings.Builder
	if opts.Width <= 0 {
		opts.Width = defaultWidth
	}
	if opts.Title == "" {
		opts.Title = "Flame Graph"
	}
	if err := renderParsed(&svg, root, maxDepth, opts); err != nil {
		return err
	}
	title := opts.Title
	_, err = fmt.Fprintf(w, "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\" />\n<title>%s</title>\n<style>body{margin:0;background:%s;background-image:linear-gradient(180deg,#fcfaf7 0%%,#f3ede4 100%%);font-family:Verdana,sans-serif;color:#201a15}main{padding:16px;max-width:1600px;margin:0 auto}.toolbar{position:sticky;top:0;z-index:10;padding:14px 16px 12px;margin-bottom:12px;border:1px solid #d9cfc1;border-radius:14px;background:rgba(255,250,244,.94);backdrop-filter:blur(8px);box-shadow:0 8px 24px rgba(82,58,35,.08)}.toolbar-top{display:flex;flex-wrap:wrap;gap:12px;align-items:center;justify-content:space-between}.title-block h1{font-size:20px;line-height:1.2;margin:0}.title-block p{margin:4px 0 0;font-size:12px;color:#6d5a49}.controls{display:flex;flex-wrap:wrap;gap:8px;align-items:center}.controls label{font-size:13px;color:#4e4033}.controls input{font:inherit;padding:8px 10px;border:1px solid #cabaa7;border-radius:10px;min-width:240px;background:#fffdf9}.controls button{font:inherit;padding:8px 10px;border:1px solid #cabaa7;border-radius:10px;background:#fff;cursor:pointer;color:#3c3026}.controls button:hover{background:#f6efe7}.status{display:flex;flex-wrap:wrap;gap:8px;margin-top:10px}.chip{display:inline-flex;gap:6px;align-items:center;padding:6px 10px;border-radius:999px;background:#efe6d8;color:#4a3a2f;font-size:12px}.chip strong{font-size:12px;color:#2a211b}.breadcrumbs{margin-top:10px;min-height:1.4em;font-size:12px;color:#5d4a3b;word-break:break-word}.hint{margin-top:6px;font-size:11px;color:#7a6553}svg{max-width:100%%;height:auto;display:block;border:1px solid #ddcfbe;border-radius:14px;background:%s;box-shadow:0 10px 28px rgba(82,58,35,.08)}#details{font-size:12px;color:#4f4135;min-height:1.4em}.frame{cursor:pointer;transition:opacity .12s ease}.frame:hover rect{stroke:#1f1a16;stroke-width:1.2}.frame.highlight rect{stroke:#111;stroke-width:1.6}.frame.dim{opacity:.14}</style>\n</head>\n<body>\n<main>\n<section class=\"toolbar\"><div class=\"toolbar-top\"><div class=\"title-block\"><h1>%s</h1><p>CPU + GPU folded flamegraph. Click to zoom, use / to search, Esc to clear or reset.</p></div><div class=\"controls\"><label>Search <input id=\"search\" type=\"search\" placeholder=\"frame name\" /></label><button id=\"clear-search\" type=\"button\">Clear Search</button><button id=\"reset-zoom\" type=\"button\">Reset Zoom</button></div></div><div class=\"status\"><div class=\"chip\"><strong>Total Samples</strong><span id=\"total-samples\"></span></div><div class=\"chip\"><strong>Visible Matches</strong><span id=\"match-count\">0</span></div><div class=\"chip\"><strong>Zoom</strong><span id=\"zoom-state\">root</span></div><div class=\"chip\"><strong>Selection</strong><span id=\"details\">Hover a frame to inspect.</span></div></div><div class=\"breadcrumbs\" id=\"breadcrumbs\">Root</div><div class=\"hint\">Tips: click a frame to focus its subtree, click the background or Reset Zoom to go back, search highlights visible frames only.</div></section>\n%s\n<script>(function(){const svg=document.querySelector('svg');if(!svg)return;const frames=[...svg.querySelectorAll('.frame')];const sidePad=%d;const svgWidth=%d;const plotWidth=svgWidth-(sidePad*2);const originalViewBox=svg.getAttribute('viewBox');const details=document.getElementById('details');const breadcrumbs=document.getElementById('breadcrumbs');const totalSamples=document.getElementById('total-samples');const matchCount=document.getElementById('match-count');const zoomState=document.getElementById('zoom-state');const search=document.getElementById('search');const clearSearch=document.getElementById('clear-search');const resetZoom=document.getElementById('reset-zoom');const total=Number(svg.dataset.total||'0');let currentZoom=null;function num(el,key){return Number(el.dataset[key]||'0');}function formatCount(v){return Number(v).toLocaleString();}function formatPct(v){if(!total)return '0%%';return ((v/total)*100).toFixed(v===total?0:2)+'%%';}function framePath(el){return (el.dataset.path||el.dataset.name||'').split(';').filter(Boolean);}function describe(el){return el.dataset.name+' · '+formatCount(num(el,'value'))+' samples · '+formatPct(num(el,'value'))+' of total';}function renderBreadcrumbs(parts){breadcrumbs.textContent=parts.length?parts.join(' › '):'Root';}function setFrame(el,x,width,visible,highlight){el.style.display=visible?'':'none';el.classList.toggle('highlight',!!highlight);el.classList.toggle('dim',false);if(!visible)return;const rect=el.querySelector('rect');const text=el.querySelector('text');rect.setAttribute('x',x.toFixed(2));rect.setAttribute('width',Math.max(0,width).toFixed(2));if(text){const textX=x+%d;text.setAttribute('x',textX.toFixed(2));text.style.display=width>=%d?'':'none';}}function updateSelection(el){if(!el){details.textContent='Hover a frame to inspect.';renderBreadcrumbs([]);return;}details.textContent=describe(el);renderBreadcrumbs(framePath(el));}function reset(){svg.setAttribute('viewBox',originalViewBox);currentZoom=null;zoomState.textContent='root';frames.forEach(el=>{setFrame(el,num(el,'origX'),num(el,'origWidth'),true,false);});updateSelection(null);applySearch(search.value);}function zoom(target){const tx=num(target,'origX');const tw=num(target,'origWidth');const tEnd=tx+tw;const ratio=plotWidth/tw;currentZoom=target;zoomState.textContent=target.dataset.name;frames.forEach(el=>{const x=num(el,'origX');const w=num(el,'origWidth');const end=x+w;const isAncestor=x<=tx&&end>=tEnd;const isDescendant=x>=tx&&end<=tEnd;const visible=isAncestor||isDescendant;if(!visible){setFrame(el,x,w,false,false);return;}if(isAncestor){setFrame(el,sidePad,plotWidth,true,false);return;}const newX=sidePad+((x-tx)*ratio);const newW=w*ratio;setFrame(el,newX,newW,true,false);});updateSelection(target);applySearch(search.value);}function applySearch(query){const q=(query||'').trim().toLowerCase();let matches=0;frames.forEach(el=>{const visible=el.style.display!=='none';const match=q!==''&&el.dataset.name.toLowerCase().includes(q);if(visible&&match)matches+=1;el.classList.toggle('highlight',match);el.classList.toggle('dim',q!==''&&visible&&!match);if(q===''){el.classList.remove('dim');}});matchCount.textContent=q===''?'0':String(matches);}frames.forEach(el=>{el.addEventListener('click',evt=>{evt.stopPropagation();zoom(el);});el.addEventListener('mouseenter',()=>updateSelection(el));el.addEventListener('mouseleave',()=>{if(currentZoom){updateSelection(currentZoom);}else{updateSelection(null);}});});svg.addEventListener('click',evt=>{if(evt.target===svg)reset();});search.addEventListener('input',e=>applySearch(e.target.value));clearSearch.addEventListener('click',()=>{search.value='';applySearch('');search.focus();});resetZoom.addEventListener('click',reset);document.addEventListener('keydown',evt=>{if(evt.key==='/'&&document.activeElement!==search){evt.preventDefault();search.focus();search.select();}if(evt.key==='Escape'){if(search.value!==''){search.value='';applySearch('');}else if(currentZoom){reset();}}});totalSamples.textContent=formatCount(total);reset();})();</script>\n</main>\n</body>\n</html>\n",
		html.EscapeString(title),
		defaultBackground,
		defaultBackground,
		html.EscapeString(title),
		svg.String(),
		defaultSidePad,
		opts.Width,
		defaultTextPad,
		defaultMinTextW)
	return err
}

func parseFolded(r io.Reader) (*node, int, error) {
	root := &node{index: make(map[string]*node)}
	scanner := bufio.NewScanner(r)
	maxDepth := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		stack, value, err := parseFoldedLine(line)
		if err != nil {
			return nil, 0, err
		}
		if len(stack) == 0 {
			return nil, 0, fmt.Errorf("empty folded stack")
		}
		if len(stack) > maxDepth {
			maxDepth = len(stack)
		}
		root.value += value
		cur := root
		for _, frame := range stack {
			if cur.index == nil {
				cur.index = make(map[string]*node)
			}
			child := cur.index[frame]
			if child == nil {
				child = &node{name: frame, index: make(map[string]*node)}
				cur.index[frame] = child
				cur.children = append(cur.children, child)
			}
			child.value += value
			cur = child
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return root, maxDepth, nil
}

func parseFoldedLine(line string) ([]string, uint64, error) {
	idx := strings.LastIndexByte(line, ' ')
	if idx <= 0 || idx == len(line)-1 {
		return nil, 0, fmt.Errorf("invalid folded line: %q", line)
	}
	stackPart := strings.TrimSpace(line[:idx])
	valuePart := strings.TrimSpace(line[idx+1:])
	value, err := strconv.ParseUint(valuePart, 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("parse folded value: %w", err)
	}
	frames := strings.Split(stackPart, ";")
	return frames, value, nil
}

func renderNode(w io.Writer, n *node, x float64, depth, maxDepth int, scale float64, parentPath string) float64 {
	width := float64(n.value) * scale
	y := float64(defaultTopPad + (maxDepth-depth-1)*defaultFrameH)
	title := html.EscapeString(fmt.Sprintf("%s (%d)", n.name, n.value))
	fill := colorFor(n.name)
	path := n.name
	if parentPath != "" {
		path = parentPath + ";" + n.name
	}

	fmt.Fprintf(w, `<g class="frame" data-name="%s" data-path="%s" data-value="%d" data-orig-x="%.2f" data-orig-width="%.2f" data-depth="%d">`+"\n", html.EscapeString(n.name), html.EscapeString(path), n.value, x, width, depth)
	fmt.Fprintf(w, `<title>%s</title>`+"\n", title)
	fmt.Fprintf(w, `<rect x="%.2f" y="%.2f" width="%.2f" height="%d" fill="%s" stroke="#e8e8e8" stroke-width="0.5" rx="2" ry="2" />`+"\n", x, y, width, defaultFrameH-1, fill)
	if width >= defaultMinTextW {
		maxChars := int(math.Floor((width - defaultTextPad*2) / 7))
		text := truncateLabel(n.name, maxChars)
		fmt.Fprintf(w, `<text x="%.2f" y="%.2f" font-size="%d" font-family="Verdana" fill="#111">%s</text>`+"\n", x+defaultTextPad, y+defaultFrameH-5, defaultFontSize, html.EscapeString(text))
	}
	fmt.Fprintf(w, `</g>`+"\n")

	childX := x
	for _, child := range n.children {
		childX = renderNode(w, child, childX, depth+1, maxDepth, scale, path)
	}
	return x + width
}

func truncateLabel(label string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(label)
	if len(runes) <= maxChars {
		return label
	}
	if maxChars <= 2 {
		return string(runes[:maxChars])
	}
	return string(runes[:maxChars-2]) + ".."
}

func colorFor(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	sum := h.Sum32()
	hue := sum % 360
	sat := 55 + (sum/360)%20
	light := 62 + (sum/7200)%12
	return fmt.Sprintf("hsl(%d, %d%%, %d%%)", hue, sat, light)
}
