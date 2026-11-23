#!/usr/bin/env python3
"""Compose the editable learning fragments into one inline lesson."""

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
    output = directory / "from-python-to-gpu.html"
    standalone = directory / "index.html"
    standalone_result = None
    if standalone.exists():
        if not output.exists():
            raise ValueError("The previous inline lesson is required to update index.html")
        previous = escape(output.read_text(encoding="utf-8"))
        document = standalone.read_text(encoding="utf-8")
        if document.count(previous) != 1:
            raise ValueError("index.html does not contain exactly one previous lesson")
        standalone_result = document.replace(previous, escape(result), 1)
    output.write_text(result, encoding="utf-8", newline="\n")
    if standalone_result is not None:
        standalone.write_text(standalone_result, encoding="utf-8", newline="\n")
    return output


if __name__ == "__main__":
    print(build())
