use std::env;
use std::ffi::{c_char, c_int, c_uint, c_void, CString};
use std::hint::black_box;
use std::ptr;
use std::thread;
use std::time::Duration;

type HipError = c_int;
type HipStream = *mut c_void;

#[repr(C)]
#[derive(Clone, Copy)]
struct Dim3 {
    x: c_uint,
    y: c_uint,
    z: c_uint,
}

type HipGetDeviceCount = unsafe extern "C" fn(*mut c_int) -> HipError;
type HipInit = unsafe extern "C" fn(c_uint) -> HipError;
type HipSetDevice = unsafe extern "C" fn(c_int) -> HipError;
type HipStreamCreate = unsafe extern "C" fn(*mut HipStream) -> HipError;
type HipStreamDestroy = unsafe extern "C" fn(HipStream) -> HipError;
type HipMalloc = unsafe extern "C" fn(*mut *mut c_void, usize) -> HipError;
type HipFree = unsafe extern "C" fn(*mut c_void) -> HipError;
type HipLaunchKernel = unsafe extern "C" fn(
    *const c_void,
    Dim3,
    Dim3,
    *mut *mut c_void,
    usize,
    HipStream,
) -> HipError;

#[link(name = "dl")]
unsafe extern "C" {
    fn dlopen(filename: *const c_char, flags: c_int) -> *mut c_void;
    fn dlsym(handle: *mut c_void, symbol: *const c_char) -> *mut c_void;
    fn dlclose(handle: *mut c_void) -> c_int;
    fn dlerror() -> *const c_char;
}

const RTLD_NOW: c_int = 2;
const RTLD_LOCAL: c_int = 0;

#[unsafe(no_mangle)]
pub extern "C" fn flash_attn_decode_bf16_gfx11() {
    std::sync::atomic::compiler_fence(std::sync::atomic::Ordering::SeqCst);
}

struct HipApi {
    handle: *mut c_void,
    get_device_count: HipGetDeviceCount,
    init: HipInit,
    set_device: HipSetDevice,
    stream_create: HipStreamCreate,
    stream_destroy: HipStreamDestroy,
    malloc: HipMalloc,
    free: HipFree,
    launch_kernel: HipLaunchKernel,
}

impl Drop for HipApi {
    fn drop(&mut self) {
        unsafe {
            let _ = dlclose(self.handle);
        }
    }
}

fn env_u64(name: &str, default: u64) -> u64 {
    env::var(name)
        .ok()
        .and_then(|v| v.parse::<u64>().ok())
        .unwrap_or(default)
}

fn env_string(name: &str, default: &str) -> String {
    env::var(name).unwrap_or_else(|_| default.to_string())
}

unsafe fn load_symbol<T: Copy>(handle: *mut c_void, symbol: &str) -> Result<T, String> {
    let c_symbol = CString::new(symbol).map_err(|e| e.to_string())?;
    let addr = dlsym(handle, c_symbol.as_ptr());
    if addr.is_null() {
        let err = dlerror();
        let msg = if err.is_null() {
            format!("missing symbol {symbol}")
        } else {
            std::ffi::CStr::from_ptr(err).to_string_lossy().into_owned()
        };
        return Err(msg);
    }
    Ok(std::mem::transmute_copy(&addr))
}

fn load_hip_api(path: &str) -> Result<HipApi, String> {
    let c_path = CString::new(path).map_err(|e| e.to_string())?;
    let handle = unsafe { dlopen(c_path.as_ptr(), RTLD_NOW | RTLD_LOCAL) };
    if handle.is_null() {
        let err = unsafe { dlerror() };
        let msg = if err.is_null() {
            format!("dlopen({path}) failed")
        } else {
            unsafe { std::ffi::CStr::from_ptr(err) }
                .to_string_lossy()
                .into_owned()
        };
        return Err(msg);
    }
    unsafe {
        Ok(HipApi {
            handle,
            get_device_count: load_symbol(handle, "hipGetDeviceCount")?,
            init: load_symbol(handle, "hipInit")?,
            set_device: load_symbol(handle, "hipSetDevice")?,
            stream_create: load_symbol(handle, "hipStreamCreate")?,
            stream_destroy: load_symbol(handle, "hipStreamDestroy")?,
            malloc: load_symbol(handle, "hipMalloc")?,
            free: load_symbol(handle, "hipFree")?,
            launch_kernel: load_symbol(handle, "hipLaunchKernel")?,
        })
    }
}

#[inline(never)]
fn cpu_spin(spin: u64) -> u64 {
    let mut acc = 0u64;
    for i in 0..spin {
        acc = acc.wrapping_add(i.rotate_left((i % 17) as u32) ^ 0x9e3779b97f4a7c15);
    }
    black_box(acc)
}

#[inline(never)]
fn launch_attention_step(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    let _ = cpu_spin(spin);
    let mut stream: HipStream = ptr::null_mut();
    let mut device_ptr: *mut c_void = ptr::null_mut();

    unsafe {
        let stream_err = (api.stream_create)(&mut stream);
        println!("hipStreamCreate -> err={stream_err} stream={stream:p}");

        let malloc_err = (api.malloc)(&mut device_ptr, 4096);
        println!("hipMalloc -> err={malloc_err} ptr={device_ptr:p}");

        let blocks = Dim3 { x: 1, y: 1, z: 1 };
        let threads = Dim3 { x: 1, y: 1, z: 1 };
        let kernel = flash_attn_decode_bf16_gfx11 as *const c_void;
        let launch_err = (api.launch_kernel)(kernel, blocks, threads, ptr::null_mut(), 0, stream);
        println!("hipLaunchKernel -> err={launch_err}");

        if !device_ptr.is_null() {
            let free_err = (api.free)(device_ptr);
            println!("hipFree -> err={free_err}");
        }
        if !stream.is_null() {
            let destroy_err = (api.stream_destroy)(stream);
            println!("hipStreamDestroy -> err={destroy_err}");
        }
    }

    thread::sleep(Duration::from_millis(sleep_ms));
    Ok(())
}

#[inline(never)]
fn flash_attention(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    launch_attention_step(api, sleep_ms, spin)
}

#[inline(never)]
fn fused_attention_residual(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    flash_attention(api, sleep_ms, spin)
}

#[inline(never)]
fn transformer_block_17(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    fused_attention_residual(api, sleep_ms, spin)
}

#[inline(never)]
fn decoder_layer_norm(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    transformer_block_17(api, sleep_ms, spin)
}

#[inline(never)]
fn paged_kv_cache_lookup(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    decoder_layer_norm(api, sleep_ms, spin)
}

#[inline(never)]
fn attention_window_update(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    paged_kv_cache_lookup(api, sleep_ms, spin)
}

#[inline(never)]
fn model_forward(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    attention_window_update(api, sleep_ms, spin)
}

#[inline(never)]
fn decode_token_step(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    model_forward(api, sleep_ms, spin)
}

#[inline(never)]
fn generate_token(api: &HipApi, sleep_ms: u64, spin: u64) -> Result<(), String> {
    decode_token_step(api, sleep_ms, spin)
}

#[inline(never)]
fn serve_request(api: &HipApi, iterations: u64, sleep_ms: u64, spin: u64) -> Result<(), String> {
    for _ in 0..iterations {
        generate_token(api, sleep_ms, spin)?;
    }
    Ok(())
}

fn main() -> Result<(), String> {
    let hip_library = env_string(
        "REAL_HIP_ATTENTION_LIBRARY",
        "/usr/local/lib/ollama/rocm/libamdhip64.so.6",
    );
    let iterations = env_u64("REAL_HIP_ATTENTION_ITERATIONS", 12);
    let sleep_before_ms = env_u64("REAL_HIP_ATTENTION_SLEEP_BEFORE_MS", 3000);
    let sleep_between_ms = env_u64("REAL_HIP_ATTENTION_SLEEP_BETWEEN_MS", 20);
    let cpu_spin_iters = env_u64("REAL_HIP_ATTENTION_CPU_SPIN", 1_500_000);

    let api = load_hip_api(&hip_library)?;
    let mut count = -1;
    unsafe {
        let count_err = (api.get_device_count)(&mut count);
        println!("hipGetDeviceCount -> err={count_err} count={count}");
        let init_err = (api.init)(0);
        println!("hipInit -> err={init_err}");
        let device_err = (api.set_device)(0);
        println!("hipSetDevice -> err={device_err}");
    }

    println!("warming up before launches for {sleep_before_ms}ms");
    thread::sleep(Duration::from_millis(sleep_before_ms));
    serve_request(&api, iterations, sleep_between_ms, cpu_spin_iters)?;
    println!("completed {iterations} iterations");
    Ok(())
}
