#!/bin/sh
# Refuse a shim that cannot load into the processes it is injected into.
#
# This library is dlopened by the CUDA driver INTO someone else's process, so
# its requirements are requirements on the TARGET, not on the machine that
# built it. Measured before this gate existed: the shipped .so needed glibc
# 2.42 and failed to dlopen on Ubuntu 22.04, 24.04 and 25.04 -- which is
# essentially every PyTorch image -- with
#
#   version `GLIBC_ABI_GNU2_TLS' not found
#
# Nothing caught it, because it loaded perfectly on the machine that built it.
#
# Checks ALL THREE version namespaces, not just glibc. A .so can clear a
# GLIBC_2.28 bar and still require GLIBCXX_3.4.31, and a single-axis gate
# would pass it.
set -eu

so=${1:?usage: elfgate.sh <shim.so> [max-glibc] [max-glibcxx]}
max_glibc=${2:-2.34}
# "none" (default): must not require libstdc++ at all, which is what a shipped
# artifact has to promise -- conda and PyTorch ship their own, older one, and a
# dynamic dependency binds theirs to ours.
# "any": permitted. For the ordinary local build, which is not the artifact and
# does not claim to be. The distinction matters: running the full gate against
# a build that never promised portability fails for a reason that is not a
# defect, and a gate that cries wolf gets switched off.
max_glibcxx=${3:-none}

fail=0
say() { printf '  %-22s %s\n' "$1" "$2"; }
bad() { printf '  FAIL %-17s %s\n' "$1" "$2"; fail=1; }

# Highest requirement in a namespace, by version sort.
maxver() {
    readelf -VW "$so" 2>/dev/null | grep -oE "$1"'_[0-9][0-9.]*' | sed "s/^$1"'_//' \
        | sort -V | tail -1
}

# GLIBC_ABI_GNU2_TLS is not a version, it is a marker, and it is the one that
# actually broke us: it is satisfied only by glibc 2.42+ regardless of every
# numeric requirement being far lower.
#
# It is an x86-64 marker. On any other architecture this grep finds nothing
# because the string is never emitted there -- not because the object is
# sound -- so reporting "absent" on aarch64 would be a check that cannot
# fail, dressed as a check that passed. It says n/a instead, and the numeric
# floor below carries the weight there: that check IS architecture-neutral,
# and a TLS dialect that raised the requirement would show up in it.
so_arch=$(readelf -hW "$so" 2>/dev/null | awk -F: '/Machine:/{print $2}' | tr -d ' ')
case "$so_arch" in
    *X86-64*|*x86-64*)
        if readelf -VW "$so" 2>/dev/null | grep -q 'GLIBC_ABI_GNU2_TLS'; then
            bad "GLIBC_ABI_GNU2_TLS" "present — needs glibc 2.42+; build with -mtls-dialect=gnu"
        else
            say "GLIBC_ABI_GNU2_TLS" "absent"
        fi
        ;;
    *)
        say "GLIBC_ABI_GNU2_TLS" "n/a (x86-64 marker; this object is $so_arch)"
        ;;
esac

g=$(maxver GLIBC)
if [ -n "$g" ] && [ "$(printf '%s\n%s\n' "$g" "$max_glibc" | sort -V | tail -1)" != "$max_glibc" ]; then
    bad "glibc" "requires $g, limit is $max_glibc"
else
    say "glibc" "requires ${g:-none} (limit $max_glibc)"
fi

x=$(maxver GLIBCXX)
if [ "$max_glibcxx" = any ]; then
    say "libstdc++" "requires ${x:-none} (permitted for this build)"
elif [ "$max_glibcxx" = none ]; then
    if [ -n "$x" ]; then
        bad "libstdc++" "requires GLIBCXX_$x; link with -static-libstdc++"
    else
        say "libstdc++" "not required"
    fi
fi

# A baked-in RUNPATH points at the BUILD machine's CUDA. In a target that has
# CUPTI somewhere else it resolves to nothing, silently.
if readelf -dW "$so" 2>/dev/null | grep -qE 'R(UN)?PATH'; then
    bad "runpath" "$(readelf -dW "$so" | grep -oE 'R(UN)?PATH.*' | head -1)"
else
    say "runpath" "none"
fi

# The injected library must export exactly its entry point. Anything else is
# preemptible against the target's own symbols -- conda and PyTorch ship their
# own libstdc++, and a leaked std:: symbol would bind theirs to ours.
n=$(nm -D --defined-only "$so" 2>/dev/null | grep -cE ' [TWi] ' || true)
if [ "$n" != 1 ]; then
    bad "exports" "$n symbols exported, want exactly 1 (InitializeInjection)"
else
    say "exports" "1 (InitializeInjection)"
fi

leak=$(nm -D --defined-only "$so" 2>/dev/null | grep -cE '_ZN?St|_ZSt|__cxa_|_Znw|_Zdl' || true)
if [ "$leak" != 0 ]; then
    bad "c++ leakage" "$leak std::/__cxa_ symbols in the dynamic table"
else
    say "c++ leakage" "none"
fi

# USDT notes are how the consumer attaches at all.
notes=$(readelf -nW "$so" 2>/dev/null | grep -c stapsdt || true)
if [ "$notes" -lt 1 ]; then
    bad "usdt notes" "none — the consumer would attach nothing"
else
    say "usdt notes" "$notes"
fi

[ "$fail" = 0 ] || { echo "  -> this shim cannot be shipped"; exit 1; }
echo "  -> portable"
