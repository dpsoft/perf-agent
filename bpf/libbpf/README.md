# Vendored libbpf headers

These are the four headers clang actually opens when compiling this repo's
BPF programs, taken from **libbpf-dev 1:1.3.0-2build2** on Ubuntu 24.04 —
the same package the CI image installs.

They are here because BPF objects were not byte-reproducible across machines
that agreed on every visible input (#117). The difference was six bytes,
inside `.BTF`'s type section, as two transpositions — a type-ordering
difference rather than a codegen one, and it reproduces when libbpf's
headers change while everything else is held constant.

Nothing else explained it. The compiler, the strip tool, the package
version and the header contents were all eliminated by measurement; see the
issue for that table. What remained was that the headers came from whatever
`/usr/include/bpf` happened to hold on the machine doing the generating.
Vendoring removes the machine from the equation: BTF ordering now depends on
files in the repo.

## Updating them

Do not copy from the local system. A developer box may carry a quite
different libbpf — Fedora 44 ships 1.6.3, which produces entirely different
objects from Ubuntu's 1.3.0. Take them from the CI image so the committed
objects stay reproducible:

```bash
podman run --rm -v "$PWD/bpf/libbpf/bpf:/out:Z" ubuntu:24.04 bash -c '
  apt-get update -qq && apt-get install -y -qq libbpf-dev
  cp -a /usr/include/bpf/bpf_core_read.h /usr/include/bpf/bpf_helper_defs.h \
        /usr/include/bpf/bpf_helpers.h   /usr/include/bpf/bpf_tracing.h /out/'
make generate-container
```

Then commit the regenerated objects alongside the headers, in the same
commit — they are one change.

## Which headers

Determined rather than guessed, with `clang -H` over the three this repo
includes directly (`bpf_helpers.h`, `bpf_core_read.h`, `bpf_tracing.h`);
`bpf_helper_defs.h` arrives transitively from the first. Adding an include
that reaches further means vendoring that header too, and `generate-check`
is what will tell you, by the objects moving.
