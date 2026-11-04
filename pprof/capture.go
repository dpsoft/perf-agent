package pprof

import "github.com/dpsoft/perf-agent/internal/capturemeta"

// CaptureWindow is the observed capture lifecycle, including attachment/setup.
type CaptureWindow = capturemeta.Window

const CaptureIDComment = capturemeta.CaptureIDComment

// SetCapture supplies metadata explicitly for existing and future builders.
// Callers must serialize this with collection.
func (b *ProfileBuilders) SetCapture(c CaptureWindow) {
	b.opt.Capture = &c
	for _, builder := range b.Builders {
		builder.opt.Capture = &c
	}
}
