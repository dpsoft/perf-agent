package capturetest_test

import (
	"os"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

func TestMain(m *testing.M) { os.Exit(capturetest.Main(m)) }
