// rust-sample is a CPU-bound Rust workload used by perf-agent's debuginfod
// integration test. The three nested functions below mirror the C fixture's
// shape (deep_function -> middle_function -> main) so the assertions in
// test/debuginfod_integration_test.go can match either binary.
//
// Compiled with `debug = "line-tables-only"` and frame pointers forced on,
// so:
//   - the FP unwinder produces a clean stack without needing eh_frame, and
//   - debuginfod-fetched DWARF resolves PCs to function names + source:line.
//
// `#[inline(never)]` keeps each function visible in the unwound stack
// even with LTO + opt-level=3.

use std::time::{Duration, Instant};

#[inline(never)]
fn deep_function(seed: u64) -> u64 {
    let mut acc = seed;
    for i in 0..1024u64 {
        acc = acc.wrapping_mul(6364136223846793005).wrapping_add(i);
    }
    std::hint::black_box(acc)
}

#[inline(never)]
fn middle_function(seed: u64) -> u64 {
    let mut total: u64 = 0;
    for i in 0..256 {
        total = total.wrapping_add(deep_function(seed.wrapping_add(i)));
    }
    std::hint::black_box(total)
}

fn main() {
    let deadline = Instant::now() + Duration::from_secs(60);
    let mut seed: u64 = 1;
    while Instant::now() < deadline {
        seed = middle_function(seed);
    }
    println!("done: {seed}");
}
