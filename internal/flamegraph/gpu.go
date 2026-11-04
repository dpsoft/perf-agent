package flamegraph

import (
	_ "embed"
	"encoding/json"
)

//go:embed gpu.css
var gpuCSS string

//go:embed gpu.js
var gpuScript string

func writeGPUView(ew *errWriter, opts Options) {
	if opts.GPUTelemetry == nil {
		return
	}
	data, err := json.Marshal(opts.GPUTelemetry)
	if err != nil {
		ew.err = err
		return
	}
	// encoding/json escapes '<', including any untrusted closing script tag.
	ew.s("<script type=\"application/json\" id=\"gpu-data\">")
	ew.s(string(data))
	ew.s("</script>\n<div class=\"gpu-tabs\" role=\"tablist\" aria-label=\"Capture views\"><button type=\"button\" role=\"tab\" id=\"gpu-tab-flame\" aria-controls=\"profile-view\" aria-selected=\"true\">Flame graph</button><button type=\"button\" role=\"tab\" id=\"gpu-tab-compare\" aria-controls=\"gpu-comparison\" aria-selected=\"false\" tabindex=\"-1\">Compare GPUs</button></div>\n")
	ew.s("<div id=\"gpu-summary-mount\"></div><section id=\"gpu-comparison\" role=\"tabpanel\" aria-labelledby=\"gpu-tab-compare\" hidden></section>\n")
	ew.s("<noscript><p class=\"note\">GPU telemetry controls require JavaScript. The profile below remains available.</p></noscript>\n")
}

func writeGPUScript(ew *errWriter, opts Options) {
	if opts.GPUTelemetry == nil {
		return
	}
	ew.s("<script>\n")
	ew.s(gpuScript)
	ew.s("\n</script>\n")
}
