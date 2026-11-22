#!/usr/bin/env python3
"""Compose the editable learning fragments into one inline lesson."""

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
    output.write_text(result, encoding="utf-8", newline="\n")
    return output


if __name__ == "__main__":
    print(build())
