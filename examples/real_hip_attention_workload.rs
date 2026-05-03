use std::env;
use std::ffi::{c_char, c_int, c_uint, c_void, CString};
use std::hint::black_box;
use std::io::Write;
use std::ptr;
use std::thread;
use std::time::Duration;

type HipError = c_int;
type HipStream = *mut c_void;
type HipModule = *mut c_void;
type HipFunction = *mut c_void;
type HiprtcProgram = *mut c_void;
type HiprtcResult = c_int;

type HipGetDeviceCount = unsafe extern "C" fn(*mut c_int) -> HipError;
type HipInit = unsafe extern "C" fn(c_uint) -> HipError;
type HipSetDevice = unsafe extern "C" fn(c_int) -> HipError;
type HipStreamCreate = unsafe extern "C" fn(*mut HipStream) -> HipError;
type HipStreamDestroy = unsafe extern "C" fn(HipStream) -> HipError;
type HipMalloc = unsafe extern "C" fn(*mut *mut c_void, usize) -> HipError;
type HipFree = unsafe extern "C" fn(*mut c_void) -> HipError;
type HipModuleLoadData = unsafe extern "C" fn(*mut HipModule, *const c_void) -> HipError;
type HipModuleGetFunction =
    unsafe extern "C" fn(*mut HipFunction, HipModule, *const c_char) -> HipError;
type HipModuleLaunchKernel = unsafe extern "C" fn(
    HipFunction,
    c_uint,
    c_uint,
    c_uint,
    c_uint,
    c_uint,
    c_uint,
    c_uint,
    HipStream,
    *mut *mut c_void,
    *mut *mut c_void,
) -> HipError;
type HipModuleUnload = unsafe extern "C" fn(HipModule) -> HipError;
type HipGetErrorString = unsafe extern "C" fn(HipError) -> *const c_char;
type HiprtcCreateProgram = unsafe extern "C" fn(
    *mut HiprtcProgram,
    *const c_char,
    *const c_char,
    c_int,
    *const *const c_char,
    *const *const c_char,
) -> HiprtcResult;
type HiprtcCompileProgram =
    unsafe extern "C" fn(HiprtcProgram, c_int, *const *const c_char) -> HiprtcResult;
type HiprtcGetCodeSize = unsafe extern "C" fn(HiprtcProgram, *mut usize) -> HiprtcResult;
type HiprtcGetCode = unsafe extern "C" fn(HiprtcProgram, *mut c_void) -> HiprtcResult;
type HiprtcDestroyProgram = unsafe extern "C" fn(*mut HiprtcProgram) -> HiprtcResult;
type HiprtcGetProgramLogSize = unsafe extern "C" fn(HiprtcProgram, *mut usize) -> HiprtcResult;
type HiprtcGetProgramLog = unsafe extern "C" fn(HiprtcProgram, *mut c_char) -> HiprtcResult;
type HiprtcGetErrorString = unsafe extern "C" fn(HiprtcResult) -> *const c_char;

#[link(name = "dl")]
unsafe extern "C" {
    fn dlopen(filename: *const c_char, flags: c_int) -> *mut c_void;
    fn dlsym(handle: *mut c_void, symbol: *const c_char) -> *mut c_void;
    fn dlclose(handle: *mut c_void) -> c_int;
    fn dlerror() -> *const c_char;
    fn _exit(status: c_int) -> !;
}

const RTLD_NOW: c_int = 2;
const RTLD_LOCAL: c_int = 0;

struct HipApi {
    handle: *mut c_void,
    get_device_count: HipGetDeviceCount,
    init: HipInit,
    set_device: HipSetDevice,
    stream_create: HipStreamCreate,
    stream_destroy: HipStreamDestroy,
    malloc: HipMalloc,
    free: HipFree,
    module_load_data: HipModuleLoadData,
    module_get_function: HipModuleGetFunction,
    module_launch_kernel: HipModuleLaunchKernel,
    module_unload: HipModuleUnload,
    get_error_string: HipGetErrorString,
    hiprtc_create_program: HiprtcCreateProgram,
    hiprtc_compile_program: HiprtcCompileProgram,
    hiprtc_get_code_size: HiprtcGetCodeSize,
    hiprtc_get_code: HiprtcGetCode,
    hiprtc_destroy_program: HiprtcDestroyProgram,
    hiprtc_get_program_log_size: HiprtcGetProgramLogSize,
    hiprtc_get_program_log: HiprtcGetProgramLog,
    hiprtc_get_error_string: HiprtcGetErrorString,
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

fn cstr_lossy_from_ptr(ptr: *const c_char) -> String {
    if ptr.is_null() {
        return "<null>".to_string();
    }
    unsafe { std::ffi::CStr::from_ptr(ptr) }
        .to_string_lossy()
        .into_owned()
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
            module_load_data: load_symbol(handle, "hipModuleLoadData")?,
            module_get_function: load_symbol(handle, "hipModuleGetFunction")?,
            module_launch_kernel: load_symbol(handle, "hipModuleLaunchKernel")?,
            module_unload: load_symbol(handle, "hipModuleUnload")?,
            get_error_string: load_symbol(handle, "hipGetErrorString")?,
            hiprtc_create_program: load_symbol(handle, "hiprtcCreateProgram")?,
            hiprtc_compile_program: load_symbol(handle, "hiprtcCompileProgram")?,
            hiprtc_get_code_size: load_symbol(handle, "hiprtcGetCodeSize")?,
            hiprtc_get_code: load_symbol(handle, "hiprtcGetCode")?,
            hiprtc_destroy_program: load_symbol(handle, "hiprtcDestroyProgram")?,
            hiprtc_get_program_log_size: load_symbol(handle, "hiprtcGetProgramLogSize")?,
            hiprtc_get_program_log: load_symbol(handle, "hiprtcGetProgramLog")?,
            hiprtc_get_error_string: load_symbol(handle, "hiprtcGetErrorString")?,
        })
    }
}

struct HipKernel {
    module: HipModule,
    function: HipFunction,
    unload: HipModuleUnload,
}

impl Drop for HipKernel {
    fn drop(&mut self) {
        unsafe {
            let _ = (self.unload)(self.module);
        }
    }
}

fn env_offload_arch() -> String {
    env::var("REAL_HIP_ATTENTION_OFFLOAD_ARCH").unwrap_or_else(|_| "gfx1103".to_string())
}

fn compile_attention_kernel(api: &HipApi) -> Result<HipKernel, String> {
    let source = CString::new(
        r#"
extern "C" __global__
void flash_attn_decode_bf16_gfx11(int* out) {
    if(threadIdx.x == 0 && out != nullptr) {
        out[0] = out[0] + 1;
    }
}
"#,
    )
    .map_err(|e| e.to_string())?;
    let prog_name = CString::new("flash_attn_decode_bf16_gfx11.cu").map_err(|e| e.to_string())?;
    let arch = CString::new(format!("--offload-arch={}", env_offload_arch())).map_err(|e| e.to_string())?;
    let stdopt = CString::new("--std=c++17").map_err(|e| e.to_string())?;
    let opts = [arch.as_ptr(), stdopt.as_ptr()];
    let mut program: HiprtcProgram = ptr::null_mut();

    unsafe {
        let create_err = (api.hiprtc_create_program)(
            &mut program,
            source.as_ptr(),
            prog_name.as_ptr(),
            0,
            ptr::null(),
            ptr::null(),
        );
        if create_err != 0 {
            return Err(format!(
                "hiprtcCreateProgram failed: {} ({create_err})",
                cstr_lossy_from_ptr((api.hiprtc_get_error_string)(create_err))
            ));
        }

        let compile_err = (api.hiprtc_compile_program)(program, opts.len() as c_int, opts.as_ptr());
        if compile_err != 0 {
            let mut log_size: usize = 0;
            let _ = (api.hiprtc_get_program_log_size)(program, &mut log_size);
            let mut log = vec![0u8; log_size.max(1)];
            let _ = (api.hiprtc_get_program_log)(program, log.as_mut_ptr().cast::<c_char>());
            let log = String::from_utf8_lossy(&log)
                .trim_end_matches('\0')
                .to_string();
            let _ = (api.hiprtc_destroy_program)(&mut program);
            return Err(format!(
                "hiprtcCompileProgram failed: {} ({compile_err}): {log}",
                cstr_lossy_from_ptr((api.hiprtc_get_error_string)(compile_err))
            ));
        }

        let mut code_size: usize = 0;
        let code_size_err = (api.hiprtc_get_code_size)(program, &mut code_size);
        if code_size_err != 0 {
            let _ = (api.hiprtc_destroy_program)(&mut program);
            return Err(format!(
                "hiprtcGetCodeSize failed: {} ({code_size_err})",
                cstr_lossy_from_ptr((api.hiprtc_get_error_string)(code_size_err))
            ));
        }

        let mut code = vec![0u8; code_size];
        let code_err = (api.hiprtc_get_code)(program, code.as_mut_ptr().cast::<c_void>());
        let destroy_err = (api.hiprtc_destroy_program)(&mut program);
        if code_err != 0 {
            return Err(format!(
                "hiprtcGetCode failed: {} ({code_err})",
                cstr_lossy_from_ptr((api.hiprtc_get_error_string)(code_err))
            ));
        }
        if destroy_err != 0 {
            return Err(format!(
                "hiprtcDestroyProgram failed: {} ({destroy_err})",
                cstr_lossy_from_ptr((api.hiprtc_get_error_string)(destroy_err))
            ));
        }

        let mut module: HipModule = ptr::null_mut();
        let module_err = (api.module_load_data)(&mut module, code.as_ptr().cast::<c_void>());
        if module_err != 0 {
            return Err(format!(
                "hipModuleLoadData failed: {} ({module_err})",
                cstr_lossy_from_ptr((api.get_error_string)(module_err))
            ));
        }

        let symbol = CString::new("flash_attn_decode_bf16_gfx11").map_err(|e| e.to_string())?;
        let mut function: HipFunction = ptr::null_mut();
        let function_err = (api.module_get_function)(&mut function, module, symbol.as_ptr());
        if function_err != 0 {
            let _ = (api.module_unload)(module);
            return Err(format!(
                "hipModuleGetFunction failed: {} ({function_err})",
                cstr_lossy_from_ptr((api.get_error_string)(function_err))
            ));
        }

        Ok(HipKernel {
            module,
            function,
            unload: api.module_unload,
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
fn launch_attention_step(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    let _ = cpu_spin(spin);
    let mut stream: HipStream = ptr::null_mut();
    let mut device_ptr: *mut c_void = ptr::null_mut();

    unsafe {
        let stream_err = (api.stream_create)(&mut stream);
        println!("hipStreamCreate -> err={stream_err} stream={stream:p}");

        let malloc_err = (api.malloc)(&mut device_ptr, 4096);
        println!("hipMalloc -> err={malloc_err} ptr={device_ptr:p}");

        let mut arg0 = device_ptr;
        let mut kernel_params = [(&mut arg0 as *mut *mut c_void).cast::<c_void>()];
        let launch_err = (api.module_launch_kernel)(
            kernel.function,
            1,
            1,
            1,
            64,
            1,
            1,
            0,
            stream,
            kernel_params.as_mut_ptr(),
            ptr::null_mut(),
        );
        println!(
            "hipModuleLaunchKernel -> err={launch_err} ({})",
            cstr_lossy_from_ptr((api.get_error_string)(launch_err))
        );

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
fn flash_attention(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    launch_attention_step(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn fused_attention_residual(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    flash_attention(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn transformer_block_17(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    fused_attention_residual(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn decoder_layer_norm(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    transformer_block_17(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn paged_kv_cache_lookup(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    decoder_layer_norm(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn attention_window_update(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    paged_kv_cache_lookup(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn model_forward(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    attention_window_update(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn decode_token_step(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    model_forward(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn generate_token(api: &HipApi, kernel: &HipKernel, sleep_ms: u64, spin: u64) -> Result<(), String> {
    decode_token_step(api, kernel, sleep_ms, spin)
}

#[inline(never)]
fn serve_request(api: &HipApi, kernel: &HipKernel, iterations: u64, sleep_ms: u64, spin: u64) -> Result<(), String> {
    for _ in 0..iterations {
        generate_token(api, kernel, sleep_ms, spin)?;
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
    let kernel = compile_attention_kernel(&api)?;

    println!("warming up before launches for {sleep_before_ms}ms");
    thread::sleep(Duration::from_millis(sleep_before_ms));
    serve_request(&api, &kernel, iterations, sleep_between_ms, cpu_spin_iters)?;
    println!("completed {iterations} iterations");
    let _ = std::io::stdout().flush();
    let _ = std::io::stderr().flush();
    unsafe { _exit(0) }
}
