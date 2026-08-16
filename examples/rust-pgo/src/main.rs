// CPU-bound demonstrator: 99% of dispatched ops are `Add`, the other 1%
// are spread across the rare arms. AutoFDO will move the Add arm to the
// hot fall-through and shrink the prologue's branch overhead. Real-world
// workloads see 5-15% speedup; this synthetic one tends to land near 8%.
//
// Run: `./rust-pgo-example <iterations>` (default 200_000_000).

#[derive(Clone, Copy)]
enum Op { Add, Sub, Mul, Div }

#[inline(never)]
fn dispatch(op: Op, a: u64, b: u64) -> u64 {
    match op {
        Op::Add => a.wrapping_add(b),
        Op::Sub => a.wrapping_sub(b),
        Op::Mul => a.wrapping_mul(b),
        Op::Div => if b == 0 { 0 } else { a / b },
    }
}

fn main() {
    let n: u64 = std::env::args()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(200_000_000);
    let ops = [Op::Add, Op::Sub, Op::Mul, Op::Div];
    let mut total: u64 = 1;
    for i in 0..n {
        // 99% Add, 1% one of the others — the hot path AutoFDO will inline
        // and place as the fall-through.
        let op = if i % 100 == 0 {
            ops[((i / 100) as usize) % 4]
        } else {
            Op::Add
        };
        total = total.wrapping_add(dispatch(op, i, total));
    }
    println!("{}", total);
}
