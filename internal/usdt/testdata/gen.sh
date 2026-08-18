#!/usr/bin/env bash
# Regenerates the USDT probe fixture binaries in this directory from the
# committed .c sources.
#
# These binaries are small, deterministic-enough test fixtures (same
# convention as this repo's committed bpf2go .o output) -- they exist so
# internal/usdt's tests do not require a compiler. Regenerate only when the
# sources here change or to refresh against a newer toolchain.
#
# Requires: gcc, and systemtap-sdt-devel (sys/sdt.h) for probe2.c/probe4.c.
# On Fedora: dnf install systemtap-sdt-devel
#
# Usage: ./gen.sh   (run from this directory, or anywhere -- it cd's here)

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

# -no-pie: fixed load addresses make readelf/objdump output easy to reason
#   about by hand, and match how the fixtures were originally authored.
# -g: keep symbols (not required by the parser, but useful for debugging
#   with objdump/readelf when regenerating).
CFLAGS="-no-pie -g"

echo "building probe (inline-asm .note.stapsdt, no systemtap header)"
# -O2: at -O0/-O1/-Os, gcc emits emit_batch's inline asm exactly once. At
# -O2/-O3 it inlines emit_batch into main *and* keeps a standalone copy,
# duplicating the asm block and therefore the .note.stapsdt note -- which is
# the point of this fixture (two probes, same provider/name, different
# location). Do not "simplify" this to -O0: that silently collapses back to
# one probe and breaks the multi-probe test.
gcc $CFLAGS -O2 -o probe probe.c

echo "building probe2 (DTRACE_PROBE1 via sys/sdt.h, no semaphore)"
gcc $CFLAGS -O0 -o probe2 probe2.c

echo "building probe4 (STAP_PROBE1 via sys/sdt.h, _SDT_HAS_SEMAPHORES)"
gcc $CFLAGS -O0 -o probe4 probe4.c

echo "done. Verify with: readelf -n probe probe2 probe4"
