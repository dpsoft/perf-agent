# `internal/cubin` fixtures

Four cubins and the three CUDA sources they are built from. The tests read the
committed `.cubin` bytes; regenerating them needs the CUDA **toolkit** but
**not** a GPU — `nvcc -cubin` compiles, it does not execute.

Generated with CUDA 13.3 (`Cuda compilation tools, release 13.3, V13.3.73`),
targeting `sm_86` (GA102 — the RTX 3090 this phase is measured on).

## Regenerating

The directory recorded in the DWARF line table is `nvcc`'s working directory,
so the fixtures are built from a fixed path to keep the bytes reproducible.
`nvcc` absolutizes the source path regardless of how it is spelled on the
command line, and `-ffile-prefix-map` does not reach the device compiler, so
there is no way to make it relative. The tests therefore compare
`filepath.Base` of the recorded file, which is stable wherever they are built.

```sh
NVCC=/usr/local/cuda-13.3/bin/nvcc
BUILD=/tmp/perf-agent-cubin-fixtures

rm -rf "$BUILD" && mkdir -p "$BUILD"
cp single.cu two_kernels.cu unrolled.cu "$BUILD"/
cd "$BUILD"

$NVCC -arch=sm_86 -lineinfo -cubin -o single_lineinfo.cubin      single.cu
$NVCC -arch=sm_86            -cubin -o single_nolineinfo.cubin   single.cu
$NVCC -arch=sm_86 -lineinfo -cubin -o two_kernels_lineinfo.cubin two_kernels.cu
$NVCC -arch=sm_86 -lineinfo -cubin -o unrolled_lineinfo.cubin    unrolled.cu

cp ./*.cubin "$OLDPWD"/
```

## What each fixture is for

| fixture | source | purpose |
| --- | --- | --- |
| `single_lineinfo.cubin` | `single.cu` | the exact `(pcOffset → line)` table of one kernel |
| `single_nolineinfo.cubin` | `single.cu` | the `-lineinfo` signal: same source, no `.debug_line` |
| `two_kernels_lineinfo.cubin` | `two_kernels.cu` | how `.debug_line` lays out across several functions |
| `unrolled_lineinfo.cubin` | `unrolled.cu` | the PC-to-line collapse ratio |

`two_kernels_lineinfo.cubin` is the one that settles a structural question, so
it is worth saying what it shows. `.debug_line` holds **one line-program
sequence per function, each starting at address 0** — not one relocated
sequence spanning the module. Both kernels are 384 bytes, so their PC ranges
are identical and the sequences are told apart only by `.rel.debug_line`.
In this fixture the relocation entries appear in the **reverse** of the
sequences' byte order in `.debug_line`, so a reader that paired sequence *i*
with relocation *i* would silently swap the two kernels' source lines.

`unrolled.cu` uses `#pragma unroll` over a 64-iteration loop. The compiler
emits a single line-table row spanning every unrolled copy of the body, so the
function's 152 instructions collapse onto 5 distinct source lines.

## Note on `nvcc` warnings

`readelf` reports `Unexpected value (…) in info field` for the `.text.*`
sections. That is NVIDIA packing per-function metadata into `sh_info`; it is
normal for a cubin and nothing here reads that field.
