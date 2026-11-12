#!/bin/sh
# Fails unless the blazesym Go binding required by go.mod and test/go.mod is
# the commit that .blazesym-ref names.
#
#   scripts/check-blazesym-pin.sh <blazesym checkout at the .blazesym-ref tag>
#
# The C library is built from the tag in .blazesym-ref and the Go binding comes
# from the module proxy at the pseudo-version in go.mod. They are two pins of
# one dependency, and a binding that does not match the library it calls can
# compile and still misread the C structs.
set -eu

src=$1
root=$(cd "$(dirname "$0")/.." && pwd)
ref=$(cat "$root/.blazesym-ref")
commit=$(git -C "$src" rev-parse "$ref^{commit}")
short=$(printf '%.12s' "$commit")

status=0
for mod in "$root/go.mod" "$root/test/go.mod"; do
  version=$(awk '$1 == "github.com/libbpf/blazesym/go" { print $2 }' "$mod")
  case $version in
    *-"$short") echo "ok: $mod requires $version ($ref)" ;;
    *)
      echo "$mod requires github.com/libbpf/blazesym/go $version, but $ref is $commit." >&2
      echo "  fix: go get github.com/libbpf/blazesym/go@$short (in each module)" >&2
      status=1 ;;
  esac
done
exit $status
