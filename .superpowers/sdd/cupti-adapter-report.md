# NVIDIA CUPTI adapter — build and GPU verification report

Branch `feat/gpu-v2-cupti-adapter`, worktree `.worktrees/gpu-v2-cupti`.
Hardware: NVIDIA GeForce RTX 3090, driver 610.57.04, CUDA 13.3, CUPTI 2026.2.1.
Date: 2026-08-20.

## What was built

| Path | What it is |
|---|---|
| `shim/nvidia/cupti_adapter.cc` | The adapter. Injected by the CUDA driver via `CUDA_INJECTION64_PATH`; turns CUPTI callbacks and activity records into the frozen USDT records of `shim/core/usdt_abi.h`. |
| `shim/nvidia/export.map` | Linker version script. `-fvisibility=hidden` alone does **not** give a single-symbol library (see below). |
| `shim/nvidia/testdata/cuda_workload.cu` | Two-kernel CUDA workload with a self-checking result and the stub's stdin-EOF linger protocol. |
| `shim/Makefile` | New targets `nvidia`, `nvidia-workload`; `clean` extended. |
| `cmd/gpu-cuda-profile/main.go` | Sibling of `cmd/gpu-stub-profile`: attaches the consumer to the adapter `.so` and runs the CUDA workload under it. |
| `.gitignore` | Ignores the built `shim/nvidia/testdata/cuda_workload`. |

`shim/core/` and `shim/stub/` are unmodified apart from the new Makefile targets.

## How it maps onto core/

Exactly the wiring `shim/stub/stub.cc` uses, with CUPTI as the event source:

- `Batch<gpu_launch_v1,32>` and `Batch<gpu_exec_v1,32>` — batched, flushed on the drain tick.
- `Sampler` — one-in-N over **every** launch; period from `PERFAGENT_GPU_SAMPLE_PERIOD` (default 8).
- `gpu_launch_sampled_v1` — emitted **unbatched, one record per probe fire**, never through a `Batch`.
- `KernelNameTable` — `intern()` on first sight from *both* the launch callback and the activity record; replayed once on the unattached→attached edge.
- `ClockFit` — `(cuptiGetTimestamp, CLOCK_MONOTONIC)` pair resampled every drain tick, under a mutex (activity buffers are decoded on a CUPTI worker thread while the drain thread resamples).
- `Drainer` — 100 ms (`PERFAGENT_GPU_DRAIN_MS`), calling `cuptiActivityFlushAll(0)`; CUPTI hands buffers over only when full, so this timer is what bounds delivery.

### Decisions taken from the spike, not re-derived

1. `InitializeInjection` carries `__attribute__((visibility("default")))` — `extern "C"` alone is hidden by `-fvisibility=hidden` and the driver then fails silently.
2. Subscribed domains: `CUPTI_CB_DOMAIN_RUNTIME_API` + `CUPTI_CB_DOMAIN_RESOURCE` only. No `DRIVER_API`.
3. `cuptiGetTimestamp` is `CLOCK_REALTIME`; conversion to CPU-monotonic is an offset through `ClockFit`. Every `*_ns` on the wire is CPU-monotonic.
4. `correlationId` is a wrapping uint32; the wire correlation is `(epoch+1) << 32 | id`, so it is never zero. Epochs advance only on the launch path (which sees ids in issue order); the activity path reads the same atomic word and steps the epoch *back* for an id that is implausibly far ahead of the last launched id — a record left over from before the most recent wrap. A backwards jump only counts as a wrap when it exceeds 2^31, so multi-threaded launch reordering does not manufacture epochs.
5. Own drain timer at 100 ms; `cuptiActivityFlushPeriod` is not used.
6. `queue_id` is zero on launches (the ABI's "unknown") and comes from `CUpti_ActivityKernel12::streamId` on execs; `device_id` from `deviceId`.

### Launch cbids

CUDA 13 routes `cudaLaunchKernel` through `__cudaLaunchKernel`. All six ids are matched:
`cudaLaunchKernel_v7000`, `_ptsz_v7000`, `cudaLaunchKernelExC_v11060`, `_ptsz_v11060`,
`__cudaLaunchKernel_v13000`, `_ptsz_v13000`. On this toolkit the older ids alone would have seen nothing.

### Semaphore gating

`on_launch` takes the launch ordinal and the sampling decision unconditionally (two relaxed atomics — that is what keeps `launch_seq` and the one-in-N period reproducible across an attach that happens mid-run) and then returns immediately if neither `gpu_launch_v1` nor `gpu_launch_sampled_v1` is armed. The name hash, the intern table's mutex and `Batch`'s mutex are all past that gate. Skipped launches are counted in `launch_unattached`; the exec path does the same with `exec_unattached`.

### Every drop is counted

`launch_unattached`, `launch_batch_dropped` (`Batch::dropped()`), `exec_unattached`, `exec_batch_dropped`, `exec_no_clock` (fit not yet seeded), `exec_no_time` (CUPTI reported `start==end==0`), `cupti_dropped` (`cuptiActivityGetNumDroppedRecords`), `buffer_alloc_failed` (an activity buffer CUPTI asked for and we could not allocate), plus `clock_steps` from `ClockFit`. All printed by the `atexit` handler when `PERFAGENT_GPU_LOG` is set.

### Logging

Silent unless `PERFAGENT_GPU_LOG` is set (the library is injected into somebody else's process). `stderr` or `-` means stderr; anything else is a file path opened append/cloexec.

## Commands and output

### Build

```
$ make -C shim nvidia
g++ -std=c++17 -O2 -Wall -Wextra -fPIC -fvisibility=hidden -I core -pthread -I core \
  -I /usr/local/cuda-13.3/targets/x86_64-linux/include -shared -o libperfagent-gpu-nvidia.so \
  nvidia/cupti_adapter.cc core/batch.cc core/clock.cc core/drain.cc core/sampler.cc \
  -Wl,--version-script=nvidia/export.map \
  -L /usr/local/cuda-13.3/targets/x86_64-linux/lib -lcupti \
  -Wl,-rpath,/usr/local/cuda-13.3/targets/x86_64-linux/lib
```

No warnings (`-Wall -Wextra`).

```
$ make -C shim nvidia-workload
/usr/local/cuda-13.3/bin/nvcc -O2 -g -lineinfo -arch=sm_86 \
  -Xcompiler -fno-omit-frame-pointer -Xcompiler -rdynamic -o nvidia/testdata/cuda_workload \
  nvidia/testdata/cuda_workload.cu
```

`-fno-omit-frame-pointer` and no strip are deliberate: the sampled launch stacks are symbolized against this binary.

### The four probes

```
$ readelf -n shim/libperfagent-gpu-nvidia.so | grep -E "Provider|Name:|Arguments"
    Provider: perfagent
    Name: gpu_launch_v1
    Arguments: 8@%rdi 8@%rsi 8@%rdx
    Provider: perfagent
    Name: gpu_exec_v1
    Arguments: 8@%rdi 8@%rsi 8@%rdx
    Provider: perfagent
    Name: gpu_kernel_name_v1
    Arguments: 8@%rdi 8@%rsi 8@%rdx
    Provider: perfagent
    Name: gpu_launch_sampled_v1
    Arguments: 8@%rdi 8@%rsi 8@%rdx
```

Parsed by the consumer's own parser (`internal/usdt.ParseFile`), which is the code path `gpuprobe.Attach` uses:

```
perfagent:gpu_launch_v1         offset=0x7b0   sem=true semoff=0x6150 args="8@%rdi 8@%rsi 8@%rdx"
perfagent:gpu_exec_v1           offset=0x7d0   sem=true semoff=0x6152 args="8@%rdi 8@%rsi 8@%rdx"
perfagent:gpu_kernel_name_v1    offset=0x918   sem=true semoff=0x6156 args="8@%rdi 8@%rsi 8@%rdx"
perfagent:gpu_launch_sampled_v1 offset=0x228d  sem=true semoff=0x6154 args="8@%rdi 8@%rsi 8@%rdx"
```

All four have semaphores, so `Attach` will not reject any of them.

### Exports

```
$ nm -D --defined-only shim/libperfagent-gpu-nvidia.so
0000000000000fb0 T InitializeInjection
```

Before the version script was added this listed 16 symbols: `std::thread::_State_impl` for `Drainer`'s lambda, `std::function`'s manager thunks, and their typeinfo/vtables — vague-linkage instantiations that `-fvisibility=hidden` cannot touch because libstdc++ declares `namespace std` with `_GLIBCXX_VISIBILITY(default)`. Several carried `perfagent::Drainer` in their mangled names, i.e. `core/` was leaking into the host process's dynamic symbol table. `nvidia/export.map` is what fixes it.

### The workload links no CUPTI

```
$ ldd shim/nvidia/testdata/cuda_workload | grep -i "cupti\|cuda"
(no output)
```

So the injection really is the driver's doing, not a link-time dependency.

### Unattached run — the requested gate

```
$ cd shim && CUDA_INJECTION64_PATH=$PWD/libperfagent-gpu-nvidia.so \
    PERFAGENT_GPU_LOG=stderr PERFAGENT_GPU_SAMPLE_PERIOD=8 \
    ./nvidia/testdata/cuda_workload 2000 200 0
perfagent-cupti: initialized pid=353853 sample_period=8 drain_ms=100 clock_offset_ns=1787179217714814364
workload: iters=2000 launches=4000 elapsed_ms=541.3 abs_err=0.000000 y[0]=2.000000
perfagent-cupti: exit pid=353853 launches=4000 sampled=500 period=8 launch_unattached=4000 \
  launch_batch_dropped=0 activity_kernels=4000 activity_other=0 buffers=6 buffer_alloc_failed=0 \
  exec_unattached=4000 exec_batch_dropped=0 exec_no_clock=0 exec_no_time=0 cupti_dropped=0 \
  names=0 clock_steps=0 clock_offset_ns=1787179217714814264
perfagent-cupti: resource cbid=1 count=1
perfagent-cupti: resource cbid=5 count=1
perfagent-cupti: resource cbid=6 count=1
perfagent-cupti: resource cbid=8 count=4000
exit=0
```

Reading it:

- `InitializeInjection` ran in a real CUDA process (the `initialized` line), and the clock fit seeded.
- 4000 launches observed = 2000 iterations x 2 kernels. Exact.
- 500 sampled = 4000 / 8. Exact, deterministic.
- **Everything dropped, nothing emitted**: `launch_unattached=4000`, `exec_unattached=4000`, `names=0` (interning is behind the name semaphore too). No probe ever fired.
- 4000 activity kernel records — one per launch, none lost by CUPTI.
- The workload still computed the right answer: `abs_err=0.000000`, exit 0.
- `clock_steps=0`; the offset moved 100 ns over the run (pure NTP slew, three orders of magnitude below the 1 ms step threshold).

Baseline for comparison, without injection: `elapsed_ms=528.2 abs_err=0.000000 y[0]=2.000000`, exit 0.

### Sampling period varies from the environment

```
$ PERFAGENT_GPU_SAMPLE_PERIOD=4 ... ./nvidia/testdata/cuda_workload 20000 0 0
launches=40000 sampled=10000 period=4 launch_unattached=40000 activity_kernels=40000
  buffers=3 exec_unattached=40000 cupti_dropped=0 clock_steps=0    (elapsed_ms=115.0)

$ PERFAGENT_GPU_SAMPLE_PERIOD=1 ... ./nvidia/testdata/cuda_workload 500 0 0
launches=1000 sampled=1000 period=1 launch_unattached=1000 activity_kernels=1000
  buffers=1 exec_unattached=1000 cupti_dropped=0 clock_steps=0     (elapsed_ms=4.0)
```

40000/4 = 10000 and 1000/1 = 1000, both exact. At 40000 launches in 115 ms — 348k launches/s — CUPTI dropped nothing.

### Unattached injection overhead

20000 iterations, no sleep, three runs each:

| | run 1 | run 2 | run 3 |
|---|---|---|---|
| no injection | 82.7 ms | 81.5 ms | 77.7 ms |
| injected, no consumer | 109.9 ms | 111.7 ms | 111.3 ms |

+37% at ~480k launches/s. Almost none of that is the adapter: past the semaphore gate its unattached path is two relaxed atomics and a branch. It is the cost of having CUPTI instrumented at all — the activity record CUPTI writes for every kernel, plus a `CUPTI_CBID_RESOURCE_MODULE_PROFILED` resource callback that fires **once per launch** (cbid 8 above: 4000 for 4000 launches, 40000 for 40000). That callback is a consequence of the RESOURCE domain subscription the spike settled on. It costs one relaxed atomic on our side; the callback dispatch itself is CUPTI's.

### Go side

```
$ go build ./...            # BUILD OK
$ go vet ./...              # clean
$ go test ./gpu/... ./gpuprobe/... ./internal/...
ok  github.com/dpsoft/perf-agent/gpu       3.569s
ok  github.com/dpsoft/perf-agent/gpuprobe  0.824s
ok  github.com/dpsoft/perf-agent/internal/bpfstack   0.006s
ok  github.com/dpsoft/perf-agent/internal/gpuabi     0.007s
ok  github.com/dpsoft/perf-agent/internal/k8slabels  0.007s
ok  github.com/dpsoft/perf-agent/internal/nspid      0.005s
ok  github.com/dpsoft/perf-agent/internal/perfdata   0.234s
ok  github.com/dpsoft/perf-agent/internal/perfevent  0.003s
ok  github.com/dpsoft/perf-agent/internal/usdt       0.158s
$ make -C shim test         # batch/clock/drain/sampler/probe_args/usdt_abi all OK
```

## What could NOT be verified here

This shell has `CapEff: 0000000000000000`, so no BPF program could be loaded and **no probe was ever armed**. Everything below is unproven and needs the privileged run:

1. That a probe fires at all. The semaphore has never been non-zero in any run above, so no `PERFAGENT_USDT_PROBE3` has executed in this library. The probe *mechanics* are proven by `shim/core/probe_args_test.cc` and by the stub's end-to-end gate, and the notes are byte-identical in structure, but this `.so` has not been observed emitting.
2. That the uprobe refcount arms probes in a process that maps the `.so` **after** attach. `cmd/gpu-cuda-profile` attaches system-wide (PID 0) because the CUDA process does not exist yet; the kernel is expected to bump the semaphore when the driver maps the library. Not exercised.
3. That the launch/exec correlation join produces matched pairs in `gpu.Timeline`. The epoch prefix and the activity-side epoch step-back are unit-reasoned, not observed against a consumer.
4. That `CUpti_ActivityKernel12`'s `start`/`end`, converted through `ClockFit`, land after the launch's `time_ns` on the same monotonic scale. The conversion runs, but nothing has read the result.
5. That sampled launch stacks symbolize to named frames of `cuda_workload` (and how far into `libcudart`/`libcuda` they reach). `-fno-omit-frame-pointer` was applied to the workload's host code only; the CUDA runtime is not built with it, so the stack may be shallow.
6. `golangci-lint` is not installed on this machine, so the repo's lint gate was not run. `go vet ./...` is clean.
7. The correlation wrap path. 2^32 ids is ~2.2 h at the spike's rate; no run here came near it.

## The privileged end-to-end run

```bash
cd /home/diego/github/perf-agent/.worktrees/gpu-v2-cupti

# 1. The adapter and the workload (no privileges needed)
make -C shim nvidia nvidia-workload

# 2. The consumer binary is already built at /home/diego/gpu-cuda-profile
#    (static blazesym; `ldd` shows only libc/libgcc). To rebuild:
#      mkdir -p /tmp/bzstatic && cp /home/diego/github/blazesym/target/release/libblazesym_c.a /tmp/bzstatic/
#      export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
#      CGO_LDFLAGS="-L/tmp/bzstatic -lblazesym_c" go build -o /home/diego/gpu-cuda-profile ./cmd/gpu-cuda-profile

# 3. Capabilities. Not /tmp — it is nosuid and file caps do not survive exec there.
sudo setcap cap_bpf,cap_perfmon+ep /home/diego/gpu-cuda-profile

# 4. Run it. No sudo.
/home/diego/gpu-cuda-profile \
  -shim   /home/diego/github/perf-agent/.worktrees/gpu-v2-cupti/shim/libperfagent-gpu-nvidia.so \
  -workload /home/diego/github/perf-agent/.worktrees/gpu-v2-cupti/shim/nvidia/testdata/cuda_workload \
  -iters 2000 -sleep-us 200 -period 8 \
  -out /home/diego/gpu-cuda.pb.gz

# 5. Read it
go tool pprof -http=: /home/diego/gpu-cuda.pb.gz
```

What a good run looks like:

- The adapter's own `exit` line should now show `launch_unattached=0`, `exec_unattached=0`, `names=2` (`perfagent_axpy` and `perfagent_scale`, mangled), and `launch_batch_dropped`/`exec_batch_dropped` near zero.
- `launches=4000 sampled=500 period=8`.
- The command's final line prints `expected_sampled=500` next to `stats=...`; `SampledLaunches` should equal it, and `StacksResolved` should be most of it.
- Non-zero `KernelDropped`, `SequenceGaps` or `Malformed` in `stats` is a real finding, not noise.

If the profile comes out empty, the first thing to check is whether the semaphore was ever armed: run the workload by hand with `PERFAGENT_GPU_LOG=stderr` while the command is attached, and see whether `launch_unattached` is still equal to `launches`.

## Concerns

1. **Nothing in this library has ever emitted a record.** Every claim above is about the unattached path. The armed path is unexercised code.
2. **`CUPTI_CBID_RESOURCE_MODULE_PROFILED` fires once per launch.** It is the only per-launch cost the RESOURCE subscription imposes, and this adapter reads none of the resource events. If the overhead matters, the subscription is the thing to revisit — but the spike settled on this set, so I did not change it.
3. **The activity-side epoch step-back is a heuristic**, correct only while the lag between a launch and its activity record is far shorter than a wrap (100 ms drain vs ~2.2 h). That holds by three orders of magnitude, but it is an assumption, not a proof.
4. **`aligned_alloc` in `buffer_requested` can return null** under memory pressure. The adapter then returns a null pointer with size 0 and counts the refusal in `buffer_alloc_failed`. CUPTI's behaviour on a declined request is **not documented** — whether it retries, drops silently, or counts the records in `cuptiActivityGetNumDroppedRecords` is unknown, and unexercised here. The refusal itself is at least never silent.
5. **`kernel_id` is an FNV-1a hash of the mangled name**, so two kernels whose names collide would merge in the profile. 64-bit FNV over a few dozen distinct names makes that vanishingly unlikely, and the alternative (a pointer-keyed cache) would not survive CUPTI handing back a different copy of the same string.
6. **Names are emitted mangled.** Demangling would need `__cxa_demangle` and a malloc on the launch path; it belongs on the consumer side.
7. `cudaGraph` launches are not covered — only the `cudaLaunchKernel` family. A graph-heavy workload would produce execs with no matching launch. `CUpti_ActivityKernel12::graphNodeId` is the hook if that becomes real.
