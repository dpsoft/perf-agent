// The module cuda_unload.cu loads and unloads, compiled on its own to a cubin
// so the host program can cuModuleLoadData it as many times as it likes.
extern "C" __global__ void spin(float *out, int n) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    float v = (float)i;
    for (int k = 0; k < 2000; k++) v = v * 1.0001f + 0.5f;
    if (i < n) out[i] = v;
}
