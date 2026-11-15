package main

import (
	"fmt"
	"html"
	"strings"

	"github.com/chromedp/cdproto/input"
)

// cpuSearch is what the CPU scene types into the flame graph's search box.
const cpuSearch = "cublas"

func (r *recorder) card(url string, seconds float64) {
	r.open(url, "body")
	r.snap(seconds)
}

// terminal types the quick start a few characters per frame.
func (r *recorder) terminal(url string) {
	r.open(url, "#t")
	set := func(h string) { r.eval(fmt.Sprintf("document.getElementById('t').innerHTML=%q", h), nil) }
	const prompt = "<span class='p'>$</span> "
	var done strings.Builder
	for _, line := range quickStart {
		for i := 4; i < len(line)+4; i += 4 {
			set(done.String() + prompt + html.EscapeString(line[:min(i, len(line))]) + "▌")
			r.frame()
		}
		done.WriteString(prompt + html.EscapeString(line) + "\n")
		set(done.String())
		r.snap(0.5)
	}
	set(done.String() + "<span class='c'>" + html.EscapeString(quickStartDone) + "</span>")
	r.snap(1.2)
}

// flamePage opens a flame graph page in the icicle layout (root on top) and
// scrolls the graph to the top of the viewport.
func (r *recorder) flamePage(url string, collapseTelemetry bool) {
	r.open(url, ".frame")
	if collapseTelemetry {
		r.eval("document.getElementById('gpu-collapse').click()", nil)
	}
	r.eval("document.getElementById('inv-btn').click()", nil)
	var top float64
	r.eval("__pa.graphTop()", &top)
	r.scrollTo(top - 10)
}

// flame is a static scene: a flame graph and its caption.
func (r *recorder) flame(url, title, sub string, seconds float64) {
	r.flamePage(url, false)
	r.caption(title, sub)
	r.snap(seconds)
}

// cpuScene sweeps the pointer over the CPU flame graph, zooms into the
// training step, resets and searches.
func (r *recorder) cpuScene(url string) {
	r.flamePage(url, false)
	r.caption("CPU", "where the time goes: Python, C++ and the CUDA runtime")
	r.call("cursor", r.mx, r.my)
	r.snap(0.8)
	var rows struct{ Top, Pitch float64 }
	r.eval(`(() => { const a = document.querySelector('.frame.d0').getBoundingClientRect(), b = document.querySelector('.frame.d1').getBoundingClientRect(); return {Top: a.top + scrollY, Pitch: b.top - a.top}; })()`, &rows)
	for i := range 10 {
		r.glide(pt{X: cssWidth * (0.14 + float64(i)*0.07), Y: rows.Top + rows.Pitch*float64(5+i%4) + rows.Pitch/2}, 2)
	}
	r.hover(`__pa.frame("pa_train_step", 0, 100, 90)`, 5)
	r.click()
	r.caption("CPU", "click to zoom into any frame")
	r.snap(1.4)
	// Click the root to zoom back out, then park the pointer right of the
	// graph so no tooltip covers the search highlights.
	r.hover(`__pa.frame("all", 0, 0, 120)`, 5)
	r.click()
	r.glide(pt{X: cssWidth - 8, Y: r.scrollY() + cssHeight*0.45}, 3)
	r.ctrlF()
	r.caption("CPU", "search any frame, see how much it weighs")
	for _, ch := range cpuSearch {
		r.key(string(ch))
		r.frame()
	}
	// Bring the widest match into view, the pointer still off the graph.
	var match float64
	r.eval(`(() => { const m = [...document.querySelectorAll('.frame.match')].sort((a, b) => b.offsetWidth - a.offsetWidth)[0]; return m ? m.getBoundingClientRect().top + scrollY : scrollY; })()`, &match)
	r.glide(pt{X: r.mx, Y: match - 100}, 6)
	r.snap(1.6)
	r.escape()
}

// fusedScene zooms into the training step on the GPU side of the fused graph
// and follows its path up to the CUDA kernel it launched.
func (r *recorder) fusedScene(url string) {
	r.flamePage(url, true)
	r.caption("CPU + GPU, one flame graph", "cmd/flamegraph -fuse: CPU samples and GPU time, side by side")
	r.call("cursor", r.mx, r.my)
	r.snap(1.0)
	// The GPU profile is the second root, so its frames start right of 50%.
	r.hover(`__pa.frame("pa_train_step", 50, 100, 90)`, 7)
	r.click()
	r.caption("CPU + GPU, one flame graph", "pa_train_step, under the GPU time it launched")
	r.snap(0.8)
	kernel := r.find(`__pa.frame("[gpu:kernel:", 0, 100, 200)`)
	r.caption("CPU + GPU, one flame graph", "Python → torch → cublas → [gpu:launch] → CUDA kernel")
	for _, name := range []string{"cublasSgemm_v2", "[gpu:launch]"} {
		r.hover(fmt.Sprintf(`__pa.over(%q, %f, 90)`, name, kernel.X), 6)
		r.snap(0.4)
	}
	r.hover(`__pa.frame("[gpu:kernel:", 0, 100, 200)`, 6)
	r.snap(2.0)
}

// telemetryScene shows the NVML summary of the fused page, then Compare GPUs.
func (r *recorder) telemetryScene() {
	r.mx, r.my = cssWidth-20, 8
	r.mouse(input.MouseMoved)
	r.call("hideCursor")
	r.escape()
	r.eval("document.getElementById('gpu-collapse').click()", nil)
	var top float64
	r.eval("document.getElementById('gpu-summary').getBoundingClientRect().top + scrollY", &top)
	r.scrollTo(top - 12)
	r.caption("GPU telemetry", "NVML: temperature, power, utilisation and clocks over the capture")
	r.snap(2.4)
	r.eval("document.getElementById('gpu-tab-compare').click()", nil)
	r.eval("document.getElementById('gpu-comparison').getBoundingClientRect().top + scrollY", &top)
	r.scrollTo(top - 12)
	r.caption("Compare GPUs", "every physical GPU over the same capture window")
	r.snap(2.0)
}

// beforeAfter types the same search into the CPU and the GPU profile: the
// share of CPU time a Python frame takes, then the share of GPU time it
// launched. The numbers in the captions are the page's own match tally.
func (r *recorder) beforeAfter(cpuURL, gpuURL, term string) {
	steps := []struct {
		url, format string
		seconds     float64
	}{
		{cpuURL, "CPU profile: %s is %s of CPU time", 2.4},
		{gpuURL, "GPU profile: the kernels %s launched are %s of GPU time", 2.8},
	}
	for _, s := range steps {
		r.flamePage(s.url, false)
		r.ctrlF()
		for _, ch := range term {
			r.key(string(ch))
		}
		var tally string
		r.eval("document.getElementById('mc').textContent", &tally)
		// "38 matched · 23.807 s · 66.45%": the last field is the share of the total.
		i := strings.LastIndex(tally, "·")
		share := strings.TrimSpace(tally[i+len("·"):])
		if i < 0 || !strings.HasSuffix(share, "%") {
			panic(errStop{fmt.Errorf("search %q: unexpected tally %q", term, tally)})
		}
		r.caption("Same Python frame, two profiles", fmt.Sprintf(s.format, term, share))
		r.snap(s.seconds)
	}
}
