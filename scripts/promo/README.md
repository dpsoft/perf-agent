# Promo video

`go run .` records perf-agent's promo video for X: a 1280x720 H.264 MP4 at
15 fps, about 43 seconds, from one real capture of a PyTorch training step.

It renders the capture with this checkout's `cmd/flamegraph`, drives the pages
in headless Chrome with real mouse and key events, holds each screenshot for a
number of frames and encodes them with ffmpeg. No screen recording: the same
capture gives the same video.

| Scene | What it shows |
|---|---|
| Title | lockup, tagline, upstream projects |
| Terminal | the quick start, typed |
| CPU | pointer sweep, click to zoom, click the root to reset, search with the match tally |
| Off-CPU | where threads block |
| GPU kernels | the GPU profile alone |
| CPU + GPU | the fused graph: zoom into `pa_train_step` on the GPU side and follow it to the CUDA kernel |
| GPU telemetry | the NVML summary, then Compare GPUs |
| Before/after | the same search in the CPU and the GPU profile; the captions quote the page's own tally |
| pprof | `go tool pprof -http` on the GPU profile |
| Closing | version, image, repository, upstream projects with their URLs |

Flame graphs are shown root on top. The generator stops if a frame the
storyboard names is not in the capture, or if the pointer is not on the frame
a caption is about.

## Requirements

Go, Chrome or Chromium (`-chrome`, default `/usr/bin/google-chrome`) and an
ffmpeg with libx264. This is its own Go module, so chromedp is not a
dependency of perf-agent.

## Capture

The 1.5.3 video used this capture (RTX 3090, CPython 3.12, torch 2.14). The
workload is a small PyTorch script, not in this repository, whose worker
thread runs a training loop: `pa_train_step` calls a matmul chain and a custom
autograd activation, `pa_activation_*`.

```bash
CUDA_INJECTION64_PATH=$SHIM PERFAGENT_GPU_SAMPLE_PERIOD=1 \
    python torch_workload.py 100000 200 0 &
./perf-agent --profile --offcpu --gpu --gpu-shim $SHIM --pid $! --duration 15s \
    --profile-output cpu.pb.gz --offcpu-output offcpu.pb.gz --gpu-output gpu.pb.gz \
    --gpu-telemetry-output telemetry.jsonl --gpu-telemetry-interval 500ms
```

The storyboard looks for `pa_train_step` and `pa_activation`, so another
workload needs those names changed in `scenes.go` and `main.go`.

## Record

```bash
cd scripts/promo
go run . -cpu cap/cpu.pb.gz -offcpu cap/offcpu.pb.gz -gpu cap/gpu.pb.gz \
    -telemetry cap/telemetry.jsonl
```

The MP4 goes to `~/.cache/promo/perf-agent-promo.mp4` (`-o`). Paths under
your home directory are shown as `~/`. `-frames dir` keeps the PNG frames;
check them, or a contact sheet, before publishing:

```bash
ffmpeg -i perf-agent-promo.mp4 \
    -vf "select='eq(n\,20)+eq(n\,212)+eq(n\,377)+eq(n\,505)',scale=640:360,tile=2x2" \
    -frames:v 1 contact.png
```
