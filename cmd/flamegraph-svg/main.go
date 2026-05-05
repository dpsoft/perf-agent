package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dpsoft/perf-agent/internal/flamegraphsvg"
)

func main() {
	title := flag.String("title", "Flame Graph", "SVG title")
	inputPath := flag.String("input", "", "Path to folded input (default: stdin)")
	outputPath := flag.String("output", "", "Path to SVG output (default: stdout)")
	htmlOutputPath := flag.String("html-output", "", "Path to standalone HTML output")
	width := flag.Int("width", 1200, "SVG width in pixels")
	flag.Parse()

	in := os.Stdin
	if *inputPath != "" {
		file, err := os.Open(*inputPath)
		if err != nil {
			fatalf("open input: %v", err)
		}
		defer file.Close()
		in = file
	}

	inputData, err := io.ReadAll(in)
	if err != nil {
		fatalf("read input: %v", err)
	}

	renderOpts := flamegraphsvg.Options{
		Title: *title,
		Width: *width,
	}

	out := os.Stdout
	if *outputPath != "" {
		file, err := os.Create(*outputPath)
		if err != nil {
			fatalf("create output: %v", err)
		}
		defer file.Close()
		out = file
	}

	if err := flamegraphsvg.Render(out, bytes.NewReader(inputData), renderOpts); err != nil {
		fatalf("render flamegraph svg: %v", err)
	}

	if *htmlOutputPath != "" {
		file, err := os.Create(*htmlOutputPath)
		if err != nil {
			fatalf("create html output: %v", err)
		}
		defer file.Close()
		if err := flamegraphsvg.RenderHTML(file, bytes.NewReader(inputData), renderOpts); err != nil {
			fatalf("render flamegraph html: %v", err)
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
