use std::env;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::Duration;

fn cpu_intensive_work(stop: Arc<AtomicBool>) {
    let mut sum = 0u64;
    while !stop.load(Ordering::Relaxed) {
        for i in 0..100000 {
            sum = sum.wrapping_add((i as f64).sqrt() as u64);
        }
    }
}

fn main() {
    let duration = env::args()
        .nth(1)
        .and_then(|s| s.parse::<u64>().ok())
        .unwrap_or(30);

    let threads = env::args()
        .nth(2)
        .and_then(|s| s.parse::<usize>().ok())
        .unwrap_or(num_cpus::get());

    println!("Rust CPU-bound workload: {} threads for {}s", threads, duration);
    println!("PID: {}", std::process::id());

    let stop = Arc::new(AtomicBool::new(false));
    let mut handles = vec![];

    for _ in 0..threads {
        let stop_clone = Arc::clone(&stop);
        handles.push(thread::spawn(move || {
            cpu_intensive_work(stop_clone);
        }));
    }

    thread::sleep(Duration::from_secs(duration));
    stop.store(true, Ordering::Relaxed);

    for handle in handles {
        handle.join().unwrap();
    }

    println!("Rust workload completed");
}
