package main

import (
	"html"
	"os"
	"path/filepath"
)

// cardCSS is the dark card the title, terminal and closing scenes share. The
// lockup's word mark is recoloured for the dark background, as its own
// prefers-color-scheme: dark rule does.
const cardCSS = `body{margin:0;height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;
background:#0d1117;color:#e6edf3;font-family:ui-sans-serif,system-ui,sans-serif}
h1{font-size:40px;margin:22px 0 8px;font-weight:650;letter-spacing:-.5px;text-align:center}
p{font-size:22px;color:#9da7b3;margin:6px;text-align:center;line-height:1.5}
code{font-family:ui-monospace,Menlo,monospace;color:#7fd9d9}
.wm{fill:#eceaf2!important}.wm-2{fill:#a29caf!important}
footer{position:fixed;bottom:26px;font-size:15px;color:#8b949e;text-align:center;line-height:1.7}
footer b{color:#c9d1d9;font-weight:600}footer code{color:#7fd9d9;font-size:14px;margin-left:4px}`

const terminalCSS = `body{margin:0;height:100vh;display:flex;align-items:center;justify-content:center;background:#0d1117}
#t{width:840px;min-height:220px;background:#161b22;border-radius:12px;padding:32px 36px;box-sizing:border-box;
font:19px/1.7 ui-monospace,Menlo,Consolas,monospace;color:#e6edf3;white-space:pre-wrap;box-shadow:0 10px 40px #000a}
.p{color:#3fbfbf}.c{color:#8b949e}`

// quickStart is what the terminal scene types: the workload started with the
// CUDA adapter, then perf-agent attached to it. The real flags, as in the
// README and docs/usage.md.
var quickStart = []string{
	"CUDA_INJECTION64_PATH=$SHIM python train.py &",
	"./perf-agent --profile --offcpu --gpu --gpu-shim $SHIM \\\n    --pid $! --gpu-telemetry --flamegraph-output auto",
}

const quickStartDone = "# CPU, off-CPU and GPU flame graphs, pprof files, NVML telemetry"

type cards struct{ title, closing, terminal string }

func writeCards(dir, repo, version string) (cards, error) {
	lockup, err := os.ReadFile(filepath.Join(repo, "docs/brand/perf-agent-lockup.svg"))
	if err != nil {
		return cards{}, err
	}
	card := func(title, subtitle, footer string) string {
		// The closing title carries the image name: smaller, so it fits one line.
		if len(title) > 50 {
			title = "<span style='font-size:34px'>" + title + "</span>"
		}
		return "<!doctype html><html><head><meta charset=utf-8><style>" + cardCSS + "</style></head><body>" +
			"<div style='transform:scale(1.6);margin-bottom:24px'>" + string(lockup) + "</div>" +
			"<h1>" + title + "</h1><p>" + subtitle + "</p><footer>" + footer + "</footer></body></html>"
	}
	var werr error
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			werr = err
		}
		return "file://" + path
	}
	c := cards{
		title: write("title.html", card("CPU and GPU profiles for running processes",
			"Python, C++ and CUDA kernels in one flame graph. Attach by PID, no restart.",
			"Powered by <b>eBPF</b>, <b>cilium/ebpf</b>, <b>blazesym</b> and <b>CUPTI</b>")),
		closing: write("closing.html", card("perf-agent "+html.EscapeString(version)+" · <code>ghcr.io/dpsoft/perf-agent</code>",
			"Linux 7.1+ · CPU, off-CPU and GPU · Python 3.12–3.14 · NVML telemetry · pprof<br>"+
				"<code>https://github.com/dpsoft/perf-agent</code>",
			"Powered by <b>eBPF</b><code>ebpf.io</code> · <b>cilium/ebpf</b><code>github.com/cilium/ebpf</code><br>"+
				"<b>blazesym</b><code>github.com/libbpf/blazesym</code> · <b>CUPTI</b><code>developer.nvidia.com/cupti</code>")),
		terminal: write("terminal.html", "<!doctype html><html><head><meta charset=utf-8><style>"+terminalCSS+
			"</style></head><body><div id='t'></div></body></html>"),
	}
	return c, werr
}
