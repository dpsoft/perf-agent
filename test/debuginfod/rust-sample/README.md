# rust-sample — release-profile fixture for the debuginfod integration test

A tiny CPU-bound Rust binary used to exercise perf-agent's symbolization
against a realistic production build profile.

## Profile (`Cargo.toml`)

```toml
[profile.release]
opt-level = 3
lto = true
codegen-units = 1
panic = "abort"
debug = "line-tables-only"
strip = "none"
frame-pointers = "force-on"
```

The interesting bits:

- `debug = "line-tables-only"` produces a much smaller `.debug_info` than
  full `debug = true`, but enough for debuginfod-fetched DWARF to resolve
  PCs to function names + source:line.
- `frame-pointers = "force-on"` keeps `%rbp` chained even at `-O3`, so
  perf-agent's FP unwinder works without DWARF CFI.
- `lto = true` + `codegen-units = 1` aggressively inline; `#[inline(never)]`
  in `src/main.rs` keeps the three named frames visible.

## Build

```bash
cargo build --release
ls target/release/rust_sample
```

If your stable toolchain rejects the `frame-pointers` profile key (it was
introduced relatively recently), drop that line and use:

```bash
RUSTFLAGS="-C force-frame-pointers=yes" cargo build --release
```

## Use with the integration test

`test/debuginfod_integration_test.go` builds this fixture, uploads its
build-id-indexed `.debug` to the docker-compose `debuginfod` server via
`../upload.sh`, spawns it, and asserts that perf-agent's resolved frames
include `deep_function` and `middle_function`.
