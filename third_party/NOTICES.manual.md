## eBPF programs and libbpf headers

The eBPF programs in `bpf/` are compiled into objects embedded in the
`perf-agent` and `gpu-cuda-profile` binaries. They declare the license `GPL`
to the kernel, which is what allows them to call GPL-only kernel helpers. Their
sources are licensed GPL-2.0-only (`third_party/licenses/bpf/LICENSE`). The
scope of that license is described in `bpf/LICENSING.md` in the source tree.

They are compiled with libbpf's `bpf_helpers.h`, `bpf_helper_defs.h`,
`bpf_tracing.h` and `bpf_core_read.h` (libbpf 1.3.0, vendored in
`bpf/libbpf/bpf/`), licensed `LGPL-2.1 OR BSD-2-Clause` and used here under
BSD-2-Clause. License texts: `third_party/licenses/libbpf/`.

## Code adapted from other projects

- `pprof/pprof.go` is derived from Grafana Pyroscope `ebpf/pprof/pprof.go`.
  Copyright 2020-2023 Grafana Labs, Apache-2.0
  (`third_party/licenses/pyroscope-ebpf/LICENSE`). Modified.
- `bpf/perf.bpf.c` is adapted from Grafana Pyroscope's eBPF profiler
  (`ebpf/bpf/`). Copyright Grafana Labs. Modified. Distributed under
  GPL-2.0-only, as described above.
- `unwind/procmap/addressmapper.go` is a port of `pfelf.AddressMapper` from
  the OpenTelemetry eBPF profiler. Copyright Elasticsearch B.V. / OpenTelemetry
  Authors, Apache-2.0 (`third_party/licenses/opentelemetry-ebpf-profiler/LICENSE`).
  Modified.

## NVIDIA headers

`perf-agent` is compiled against the NVML header `nvml.h`, shipped in the
`github.com/NVIDIA/go-nvml` module. The CUDA shim (`libperfagent-gpu-nvidia.so`,
the `perf-agent-shim` image) is compiled against CUPTI headers from the CUDA
Toolkit. No NVIDIA library is linked into or shipped with either artifact: NVML
and CUPTI are loaded at run time from the host's driver and CUDA installation.

Both headers require their disclaimer and U.S. Government End Users notice to
be reproduced in user documentation. The full notices are in
`third_party/licenses/nvidia/NVML-NOTICE` and
`third_party/licenses/nvidia/CUPTI-NOTICE`. Their disclaimer and U.S.
Government End Users notice read:

> NVIDIA MAKES NO REPRESENTATION ABOUT THE SUITABILITY OF THIS SOURCE CODE FOR
> ANY PURPOSE. IT IS PROVIDED "AS IS" WITHOUT EXPRESS OR IMPLIED WARRANTY OF
> ANY KIND. NVIDIA DISCLAIMS ALL WARRANTIES WITH REGARD TO THIS SOURCE CODE,
> INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY, NONINFRINGEMENT, AND
> FITNESS FOR A PARTICULAR PURPOSE. IN NO EVENT SHALL NVIDIA BE LIABLE FOR ANY
> SPECIAL, INDIRECT, INCIDENTAL, OR CONSEQUENTIAL DAMAGES, OR ANY DAMAGES
> WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION
> OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN
> CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOURCE CODE.
>
> U.S. Government End Users. This source code is a "commercial item" as that
> term is defined at 48 C.F.R. 2.101 (OCT 1995), consisting of "commercial
> computer software" and "commercial computer software documentation" as such
> terms are used in 48 C.F.R. 12.212 (SEPT 1995) and is provided to the U.S.
> Government only as a commercial end item. Consistent with 48 C.F.R.12.212
> and 48 C.F.R. 227.7202-1 through 227.7202-4 (JUNE 1995), all U.S. Government
> End Users acquire the source code with only those rights set forth herein.

## GCC runtime libraries (CUDA shim)

The shim links libstdc++ and libgcc statically (`-static-libstdc++
-static-libgcc`). They are licensed GPL-3.0 with the GCC Runtime Library
Exception 3.1, which permits distributing the combined work under terms of the
distributor's choice when it is compiled with GCC, as the shim is. Exception
text: `third_party/licenses/gcc/COPYING.RUNTIME`.

## Container base images

`perf-agent-shim` is built on `busybox:stable` (BusyBox 1.36.1, the Docker
Official Image). BusyBox is licensed GPL-2.0 (`third_party/licenses/busybox/COPYING`).
The image redistributes the unmodified BusyBox binary; its corresponding source
is the BusyBox 1.36.1 release at https://busybox.net/downloads/busybox-1.36.1.tar.bz2,
and the build recipe is at https://github.com/docker-library/busybox.

`perf-agent` (the GPU profiler image) is built on `ubuntu:24.04` with
`libcap2-bin` and `ca-certificates` added. Every Ubuntu package keeps its
copyright and license file under `/usr/share/doc/<package>/copyright` in the
image. Source for every package is available from the Ubuntu archive
(https://packages.ubuntu.com/noble/, `apt-get source <package>`).
