#!/usr/bin/env bash
# Builds minidebug.so: a stripped shared object whose eval-loop symbols live
# ONLY in a .gnu_debugdata (MiniDebugInfo) section, the shape Fedora ships.
#
# Models Fedora 44's libpython3.14.so.1.0 (issue #170): the LTO build splits
# _PyEval_EvalFrameDefault so .dynsym shows 994 bytes -- below pyunwind's
# 8192-byte floor -- while the 80785-byte bulk sits in a .cold fragment that
# only the stripped-away .symtab names. Sizes are declared with .size over a
# one-instruction body so the fixture stays a few KB rather than 80.
#
# Regenerate: ./gen-minidebug.sh  (needs gcc, objcopy, xz)
set -euo pipefail
cd "$(dirname "$0")"

cat > /tmp/minidebug-src.s <<'ASM'
        .text
        .globl  _PyEval_EvalFrameDefault
        .type   _PyEval_EvalFrameDefault,@function
_PyEval_EvalFrameDefault:
        ret
        .size   _PyEval_EvalFrameDefault, 994

        # LOCAL, deliberately: on Fedora the .cold fragment is a local symbol,
        # so --strip-all removes it from the shipped object entirely and it
        # survives only inside .gnu_debugdata. If it were .globl it would stay
        # in .dynsym and this fixture would pass WITHOUT the MiniDebugInfo
        # path -- a test that cannot fail.
        .type   _PyEval_EvalFrameDefault.cold,@function
_PyEval_EvalFrameDefault.cold:
        ret
        .size   _PyEval_EvalFrameDefault.cold, 80785

        .globl  PyGILState_GetThisThreadState
        .type   PyGILState_GetThisThreadState,@function
PyGILState_GetThisThreadState:
        ret
        .size   PyGILState_GetThisThreadState, 64
ASM

gcc -shared -nostdlib -o /tmp/minidebug-full.so /tmp/minidebug-src.s

# The MiniDebugInfo image: symbols only, exactly as `find-debuginfo` builds it.
objcopy --only-keep-debug /tmp/minidebug-full.so /tmp/minidebug-dbg.so
xz -f --keep --stdout /tmp/minidebug-dbg.so > /tmp/minidebug-dbg.so.xz

# The shipped object: stripped, with the compressed symbols bolted back on.
objcopy --strip-all /tmp/minidebug-full.so minidebug.so
objcopy --add-section .gnu_debugdata=/tmp/minidebug-dbg.so.xz minidebug.so

echo "wrote $(pwd)/minidebug.so ($(stat -c%s minidebug.so) bytes)"
readelf -SW minidebug.so | grep -E 'gnu_debugdata|symtab' || true
