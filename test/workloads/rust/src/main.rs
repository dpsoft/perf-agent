use std::env;
use std::ffi::{CString, c_void};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicPtr, Ordering};
use std::thread;
use std::time::Duration;

type ProbeSpinFn = unsafe extern "C" fn(u64) -> u64;

#[inline(never)]
fn cpu_intensive_work(stop: Arc<AtomicBool>, probe: Arc<AtomicPtr<c_void>>) {
    let mut sum = 0u64;
    while !stop.load(Ordering::Relaxed) {
        let p = probe.load(Ordering::Relaxed);
        if !p.is_null() {
            let f: ProbeSpinFn = unsafe { std::mem::transmute(p) };
            unsafe { sum = sum.wrapping_add(f(100_000)); }
        } else {
            for i in 0..100_000u64 {
                sum = sum.wrapping_add((i as f64).sqrt() as u64);
            }
        }
    }
    std::hint::black_box(sum);
}

fn main() {
    let args: Vec<String> = env::args().collect();
    let duration = args.iter().nth(1).and_then(|s| s.parse::<u64>().ok()).unwrap_or(30);
    let threads = args.iter().nth(2).and_then(|s| s.parse::<usize>().ok()).unwrap_or(num_cpus::get());
    let dlopen_path = args.iter().position(|a| a == "--dlopen").and_then(|i| args.get(i+1)).cloned();
    let dlopen_delay = args.iter().position(|a| a == "--dlopen-delay")
        .and_then(|i| args.get(i+1))
        .and_then(|s| s.parse::<u64>().ok())
        .unwrap_or(0);

    println!("Rust CPU-bound workload: {} threads for {}s", threads, duration);
    println!("PID: {}", std::process::id());
    let stop = Arc::new(AtomicBool::new(false));
    let probe = Arc::new(AtomicPtr::<c_void>::new(std::ptr::null_mut()));
    let mut handles = vec![];
    for _ in 0..threads {
        let stop_clone = Arc::clone(&stop);
        let probe_clone = Arc::clone(&probe);
        handles.push(thread::spawn(move || cpu_intensive_work(stop_clone, probe_clone)));
    }

    if let Some(path) = dlopen_path {
        if dlopen_delay > 0 {
            println!("delaying dlopen by {}s", dlopen_delay);
            thread::sleep(Duration::from_secs(dlopen_delay));
        }
        unsafe {
            let c = CString::new(path.as_str()).unwrap();
            let h = libc::dlopen(c.as_ptr(), libc::RTLD_NOW);
            if h.is_null() {
                eprintln!("dlopen({}) failed", path);
                std::process::exit(2);
            }
            let sym = CString::new("probe_spin").unwrap();
            let ptr = libc::dlsym(h, sym.as_ptr());
            if ptr.is_null() {
                eprintln!("dlsym(probe_spin) failed");
                std::process::exit(2);
            }
            probe.store(ptr, Ordering::Release);
        }
        println!("dlopened {}", path);
    }

    thread::sleep(Duration::from_secs(duration));
    stop.store(true, Ordering::Relaxed);
    for h in handles { h.join().unwrap(); }
    println!("Rust workload completed");
}
