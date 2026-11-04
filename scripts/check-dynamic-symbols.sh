#!/bin/sh
# go-nvml suppresses link errors for unresolved symbols globally. Reject any
# unexpected strong import instead of allowing a broken binary to ship.
set -eu
if [ "$#" -eq 0 ]; then
    echo "usage: $0 <ELF binary>..." >&2
    exit 2
fi
for binary in "$@"; do
    symbols=$(readelf --dyn-syms --wide "$binary")
    printf '%s\n' "$symbols" | awk -v binary="$binary" '
        $7 == "UND" && $5 != "WEAK" && $8 != "" &&
        $8 !~ /^nvml/ && $8 !~ /@(GLIBC_|GCC_)/ {
            print binary ": unexpected unresolved symbol: " $8 > "/dev/stderr"
            bad = 1
        }
        END { exit bad ? 1 : 0 }
    '
done
