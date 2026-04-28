package gpu

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func WriteFoldedStacks(w io.Writer, snap Snapshot) error {
	for _, line := range foldedLines(snap) {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func foldedLines(snap Snapshot) []string {
	var out []string
	for _, sample := range ProjectExecutionSamples(snap) {
		if len(sample.Stack) == 0 {
			continue
		}
		out = append(out, foldedLine(ppFrameNames(sample.Stack), sample.Value))
	}
	for _, view := range snap.EventViews {
		stack := buildEventStack(view)
		if len(stack) == 0 {
			continue
		}
		out = append(out, foldedLine(stack, max(1, view.Event.DurationNs)))
	}
	return out
}

func buildEventStack(view EventView) []string {
	if view.Launch == nil {
		return nil
	}
	names := ppFrameNames(view.Launch.Launch.CPUStack)
	names = append(names, ppFrameNames(buildLaunchTagFrames(view.Launch.Launch.Tags))...)
	names = append(names, "[gpu:launch]")
	names = append(names, fmt.Sprintf("[gpu:event:%s:%s]", view.Event.Kind, view.Event.Name))
	return names
}

func ppFrameNames(frames []pp.Frame) []string {
	names := make([]string, 0, len(frames))
	for _, frame := range frames {
		names = append(names, frame.Name)
	}
	return names
}

func foldedLine(frames []string, value uint64) string {
	return fmt.Sprintf("%s %s", strings.Join(frames, ";"), strconv.FormatUint(value, 10))
}
