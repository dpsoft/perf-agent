#!/usr/bin/env python3

"""
Toy CPU+GPU-shaped decode example used to name the richer flamegraph fixture.

This is not the source of the checked-in GPU samples. It exists so the richer
CPU stack in the offline demo maps to a plausible application structure:

    serve_request -> generate_token -> model_forward
      -> transformer_block_17 -> flash_attention -> hipLaunchKernel

The replay and rocprofiler-sdk fixtures use the same names so the rendered
CPU+GPU flamegraph looks like an inference path instead of a synthetic shim.
"""


def hipLaunchKernel(kernel_name: str) -> dict[str, str]:
    return {"kernel_name": kernel_name, "queue": "compute:0"}


def flash_attention() -> dict[str, str]:
    return hipLaunchKernel("flash_attn_decode_bf16_gfx11")


def transformer_block_17() -> dict[str, str]:
    return flash_attention()


def model_forward() -> dict[str, str]:
    return transformer_block_17()


def generate_token() -> dict[str, str]:
    return model_forward()


def serve_request() -> dict[str, str]:
    return generate_token()


if __name__ == "__main__":
    print(serve_request())
