// single.cu — one kernel, a handful of distinct source lines.
//
// Built for both single_lineinfo.cubin and single_nolineinfo.cubin;
// see README.md in this directory for the exact nvcc invocations.
extern "C" __global__ void addOne(float *out, const float *in, int n) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i < n) {
        float v = in[i];
        v = v + 1.0f;
        out[i] = v;
    }
}
