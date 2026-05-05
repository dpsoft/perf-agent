package flamegraphsvg

import (
	"strings"
	"testing"
)

func TestRenderProducesSVGForFoldedStacks(t *testing.T) {
	input := strings.NewReader(
		"root;branch_a;leaf_x 3\n" +
			"root;branch_b;leaf_y 5\n",
	)

	var out strings.Builder
	if err := Render(&out, input, Options{Title: "GPU Flame Graph"}); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"<svg",
		"GPU Flame Graph",
		"branch_a",
		"leaf_x",
		"branch_b",
		"leaf_y",
		"<rect",
		"<title>leaf_y (5)</title>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestRenderIncludesRichGPUFrames(t *testing.T) {
	input := strings.NewReader(
		"train_step;hipLaunchKernel;[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:flash_attn_fwd];[gpu:stall:memory_wait];[gpu:function:flash_attn_epilogue];[gpu:source:flash_attn.hip:77];[gpu:pc:0xabc] 11\n",
	)

	var out strings.Builder
	if err := Render(&out, input, Options{}); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"[gpu:kernel:flash_attn_fwd]",
		"[gpu:function:flash_attn_epilogue]",
		"[gpu:source:flash_attn.hip:77]",
		"[gpu:pc:0xabc]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestRenderRejectsMalformedFoldedLine(t *testing.T) {
	input := strings.NewReader("not-a-valid-line\n")
	var out strings.Builder
	if err := Render(&out, input, Options{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRenderHTMLWrapsSVGDocument(t *testing.T) {
	input := strings.NewReader("root;leaf 5\n")

	var out strings.Builder
	if err := RenderHTML(&out, input, Options{Title: "GPU HTML Flame"}); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"<!DOCTYPE html>",
		"<html",
		"<title>GPU HTML Flame</title>",
		"<svg",
		"GPU HTML Flame",
		"leaf",
		`id="search"`,
		`id="reset-zoom"`,
		`id="match-count"`,
		`id="breadcrumbs"`,
		`Click to zoom`,
		`querySelectorAll('.frame')`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestRenderHTMLAnnotatesFramesForNavigation(t *testing.T) {
	input := strings.NewReader("root;branch;leaf 7\n")

	var out strings.Builder
	if err := RenderHTML(&out, input, Options{Title: "GPU HTML Flame"}); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		`class="frame"`,
		`data-name="leaf"`,
		`data-path="root;branch;leaf"`,
		`data-orig-x="`,
		`data-orig-width="`,
		`function zoom(target)`,
		`function applySearch(query)`,
		`function renderBreadcrumbs(parts)`,
		`evt.key==='/'`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}
