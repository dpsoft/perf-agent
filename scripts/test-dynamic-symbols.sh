#!/bin/sh
set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
fixture_dir=$(mktemp -d)
trap 'rm -r "$fixture_dir"' EXIT HUP INT TERM
cat > "$fixture_dir/probe.c" <<'C'
extern int nvmlProbe(void);
int main(void) { return nvmlProbe(); }
C
cc -Wl,--unresolved-symbols=ignore-in-object-files -Wl,--export-dynamic "$fixture_dir/probe.c" -o "$fixture_dir/nvml"
sh "$script_dir/check-dynamic-symbols.sh" "$fixture_dir/nvml"
sed s/nvmlProbe/missingDependency/g "$fixture_dir/probe.c" > "$fixture_dir/broken.c"
cc -Wl,--unresolved-symbols=ignore-in-object-files -Wl,--export-dynamic "$fixture_dir/broken.c" -o "$fixture_dir/broken"
if sh "$script_dir/check-dynamic-symbols.sh" "$fixture_dir/broken" > "$fixture_dir/error" 2>&1; then
    echo "unresolved non-NVML symbol was accepted" >&2
    exit 1
fi
grep -q missingDependency "$fixture_dir/error"
