#!/usr/bin/env python3
"""Compose the editable lessons and synchronize existing browser exports."""

from html import escape
from pathlib import Path


def build() -> Path:
    directory = Path(__file__).resolve().parent
    shell = (directory / "fragments/course-shell.html").read_text(encoding="utf-8")
    model = (directory / "fragments/execution-model.html").read_text(encoding="utf-8")
    marker = "<!-- MODEL_FRAGMENT -->"
    if shell.count(marker) != 1:
        raise ValueError("Course shell must contain exactly one model placeholder")
    if 'id="pa-gpu-lesson-one"' not in model:
        raise ValueError("Execution model root is missing")
    result = shell.replace(marker, model)
    if len(result.encode("utf-8")) >= 1_000_000:
        raise ValueError("Inline lesson exceeds the rendering size limit")
    warps = (directory / "fragments/warp-model.html").read_text(encoding="utf-8")
    if 'id="pa-warp-learning"' not in warps:
        raise ValueError("Warp model root is missing")
    if len(warps.encode("utf-8")) >= 1_000_000:
        raise ValueError("Warp lesson exceeds the rendering size limit")
    # Validate every existing export before changing any output. The wrappers
    # are produced once by the visualization renderer; rebuilds need stdlib only.
    writes = []
    for inline_name, browser_name, fragment in (
        ("from-python-to-gpu.html", "index.html", result),
        ("warps-and-latency-hiding.html", "warps.html", warps),
    ):
        output, standalone = directory / inline_name, directory / browser_name
        if standalone.exists():
            if not output.exists():
                raise ValueError(f"The previous inline lesson is required to update {browser_name}")
            previous = escape(output.read_text(encoding="utf-8"))
            document = standalone.read_text(encoding="utf-8")
            if document.count(previous) != 1:
                raise ValueError(f"{browser_name} does not contain exactly one previous lesson")
            writes.append((standalone, document.replace(previous, escape(fragment), 1)))
        writes.append((output, fragment))
    for output, content in writes:
        output.write_text(content, encoding="utf-8", newline="\n")
    return directory / "from-python-to-gpu.html"


if __name__ == "__main__":
    print(build())
