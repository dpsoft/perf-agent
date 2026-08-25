// two_kernels.cu — two independent kernels in one module, so the layout of
// .debug_line across several functions can be asserted rather than assumed
// (one sequence per function, each starting at address 0, versus one
// relocated sequence covering the module).
extern "C" __global__ void scale(float *out, const float *in, float k, int n) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i < n) {
        float v = in[i];
        out[i] = v * k;
    }
}

extern "C" __global__ void offset(float *out, const float *in, float b, int n) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i < n) {
        float v = in[i];
        float w = v + b;
        float z = w * w;
        out[i] = z + b;
    }
}
