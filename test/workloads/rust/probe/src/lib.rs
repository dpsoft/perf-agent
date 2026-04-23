#[inline(never)]
#[no_mangle]
pub extern "C" fn probe_spin(iters: u64) -> u64 {
    let mut sum: u64 = 0;
    for i in 0..iters {
        sum = sum.wrapping_add((i as f64).sqrt() as u64);
    }
    sum
}
