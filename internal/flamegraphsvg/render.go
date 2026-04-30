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

	if _, err := fmt.Fprintf(w, `<svg version="1.1" width="%d" height="%d" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg">`+"\n", opts.Width, canvasHeight, opts.Width, canvasHeight); err != nil {
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
		x = renderNode(w, child, x, 0, maxDepth, scale)
	}

	_, err = io.WriteString(w, "</svg>\n")
	return err
}

func RenderHTML(w io.Writer, folded io.Reader, opts Options) error {
	var svg strings.Builder
	if err := Render(&svg, folded, opts); err != nil {
		return err
	}
	title := opts.Title
	if title == "" {
		title = "Flame Graph"
	}
	_, err := fmt.Fprintf(w, "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\" />\n<title>%s</title>\n<style>body{margin:0;background:%s;font-family:Verdana,sans-serif}main{padding:12px}svg{max-width:100%%;height:auto;display:block}</style>\n</head>\n<body>\n<main>\n%s</main>\n</body>\n</html>\n", html.EscapeString(title), defaultBackground, svg.String())
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

func renderNode(w io.Writer, n *node, x float64, depth, maxDepth int, scale float64) float64 {
	width := float64(n.value) * scale
	y := float64(defaultTopPad + (maxDepth-depth-1)*defaultFrameH)
	title := html.EscapeString(fmt.Sprintf("%s (%d)", n.name, n.value))
	fill := colorFor(n.name)

	fmt.Fprintf(w, `<g>`+"\n")
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
		childX = renderNode(w, child, childX, depth+1, maxDepth, scale)
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
