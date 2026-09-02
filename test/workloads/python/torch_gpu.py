#!/usr/bin/env python3
"""Real PyTorch GPU workload for perf-agent's Python-frame walker.

Shaped deliberately so a profile can prove three things that synthetic
workloads cannot:

  1. GPU-attributed stacks carry PYTHON frames, not just native ones. The
     CUDA launches come from torch's C++ dispatcher, so the Python frames
     above it only appear if the interpreter walk actually ran.

  2. The walk survives a Python -> C extension -> Python boundary. A custom
     autograd Function puts real Python (its backward) underneath torch's
     C++ autograd engine, which is underneath more Python. A walker that
     restarts the chain instead of resuming it renders the inner segment
     twice, in a plausible order.

  3. Frames come from a NON-MAIN thread. The training loop runs in a worker,
     so a walker that can only reach the main thread's PyThreadState finds
     nothing here.

Same CLI and linger contract as shim/nvidia/testdata/cuda_workload.cu:
argv = <iters> <sleep_us> <linger_ms>, and we stay alive until stdin hits
EOF because our /proc/<pid>/maps is what the consumer symbolizes against.
"""
import os
import sys
import select
import threading
import time

try:
    import torch
except ImportError:  # not a build dependency; the profiler does not need it
    print("torch not installed; this workload is optional. "
          "Install with: uv venv --python 3.12 && uv pip install torch",
          file=sys.stderr)
    raise SystemExit(0)

SIZE = 2048


class PaCustomActivation(torch.autograd.Function):
    """Puts Python on both sides of torch's C++ autograd engine."""

    @staticmethod
    def forward(ctx, x):
        ctx.save_for_backward(x)
        return pa_activation_forward_math(x)

    @staticmethod
    def backward(ctx, grad_out):
        (x,) = ctx.saved_tensors
        return pa_activation_backward_math(grad_out, x)


def pa_activation_forward_math(x):
    return torch.tanh(x) * torch.sigmoid(x)


def pa_activation_backward_math(grad_out, x):
    t = torch.tanh(x)
    s = torch.sigmoid(x)
    return grad_out * (s * (1 - t * t) + t * s * (1 - s))


def pa_matmul_chain(a, b):
    # Deep enough that the GPU is genuinely busy: a 3090 eats a 512x512
    # matmul faster than Python can enqueue the next one, which produces a
    # profile of interpreter startup rather than of the work.
    h = torch.matmul(a, b)
    for _ in range(6):
        h = torch.matmul(h, b)
        h = torch.relu(h)
    return torch.matmul(h, b.t())


def pa_forward_block(a, b):
    h = pa_matmul_chain(a, b)
    return PaCustomActivation.apply(h)


def pa_train_step(a, b, opt):
    opt.zero_grad(set_to_none=True)
    out = pa_forward_block(a, b)
    loss = out.sum()
    loss.backward()
    opt.step()
    return loss


def pa_worker_loop(iters, sleep_us, dev):
    torch.manual_seed(0)
    a = torch.randn(SIZE, SIZE, device=dev, requires_grad=True)
    b = torch.randn(SIZE, SIZE, device=dev, requires_grad=True)
    opt = torch.optim.SGD([a, b], lr=1e-4)
    for i in range(iters):
        pa_train_step(a, b, opt)
        if sleep_us:
            time.sleep(sleep_us / 1e6)
    torch.cuda.synchronize()


def linger(linger_ms):
    if not linger_ms:
        return
    deadline = time.monotonic() + linger_ms / 1000.0
    while True:
        left = deadline - time.monotonic()
        if left <= 0:
            return
        r, _, _ = select.select([sys.stdin], [], [], left)
        if not r:
            return
        if not os.read(sys.stdin.fileno(), 256):
            return


def main():
    iters = int(sys.argv[1]) if len(sys.argv) > 1 else 2000
    sleep_us = int(sys.argv[2]) if len(sys.argv) > 2 else 200
    linger_ms = int(sys.argv[3]) if len(sys.argv) > 3 else 0

    if not torch.cuda.is_available():
        print("no CUDA device", file=sys.stderr)
        return 1
    dev = torch.device("cuda:0")
    print(f"torch {torch.__version__} on {torch.cuda.get_device_name(0)}", flush=True)

    # Warm up on the MAIN thread so CUDA context creation and torch's lazy
    # module imports are not attributed to the worker we care about.
    torch.randn(8, 8, device=dev).sum().item()

    t = threading.Thread(target=pa_worker_loop, args=(iters, sleep_us, dev),
                         name="pa-worker", daemon=False)
    t.start()
    print("READY", flush=True)
    t.join()
    print("done", flush=True)
    linger(linger_ms)
    return 0


if __name__ == "__main__":
    sys.exit(main())
