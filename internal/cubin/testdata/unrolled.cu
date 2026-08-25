// unrolled.cu — a fully unrolled loop. Every unrolled copy of the body carries
// the same source line, so distinct PCs greatly outnumber distinct lines. This
// is the fixture that makes the plan's PC-to-line collapse claim assertable.
extern "C" __global__ void unrolledSum(float *out, const float *in, int n) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    float acc = 0.0f;
#pragma unroll
    for (int k = 0; k < 64; ++k) {
        acc += in[i * 64 + k] * (float)(k + 1);
    }
    out[i] = acc;
}
