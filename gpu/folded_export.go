package gpu

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

func WriteFoldedStacks(w io.Writer, snap Snapshot) error {
	samples := ProjectExecutionSamples(snap)
	for _, sample := range samples {
		if len(sample.Stack) == 0 {
			continue
		}
		names := make([]string, 0, len(sample.Stack))
		for _, frame := range sample.Stack {
			names = append(names, frame.Name)
		}
		if _, err := fmt.Fprintf(w, "%s %s\n", strings.Join(names, ";"), strconv.FormatUint(sample.Value, 10)); err != nil {
			return err
		}
	}
	return nil
}
