# Rust Profiling Guide

## Debug Symbols in Release Builds

By default, Rust **release builds strip debug symbols** for smaller binary sizes. This means profiling tools won't be able to show function names.

### The Problem

**Without debug symbols:**
```
Found symbol: e400007f3537ae36: <no-symbol> (file: )
```
Only memory addresses, no function names.

**With debug symbols:**
```
Found symbol: rust_workload::cpu_intensive_work (file: main.rs)
Found symbol: rust_workload::main (file: main.rs)
```
Proper function names and file locations!

## Solution: Enable Debug Symbols in Release Builds

Our Rust test workload is configured to include debug symbols while maintaining release optimizations.

### Configuration

In `test/workloads/rust/Cargo.toml`:

```toml
[profile.release]
debug = true   # Include debug information
strip = false  # Don't strip symbols
```

This gives you:
- ✅ **Release optimizations** - Fast performance
- ✅ **Debug symbols** - Proper symbolization in profiles
- ✅ **Best of both worlds**

### Build Output

With debug symbols enabled, you'll see:

```bash
$ cargo build --release
Finished `release` profile [optimized + debuginfo] target(s)
                            ^^^^^^^^^^^^^^^^^^^^^^
                            Note the "+ debuginfo"
```

### Verification

Check if binary has debug symbols:

```bash
# Check file type
file test/workloads/rust/target/release/rust-workload
# Output: ... with debug_info, not stripped

# Check for symbols
nm test/workloads/rust/target/release/rust-workload | grep main
# Output: 0000000000008290 T main
```

## Binary Size Impact

Including debug symbols increases binary size:

| Configuration | Size | Profiling |
|---------------|------|-----------|
| Release (default) | ~500 KB | ❌ No symbols |
| Release + debug | ~2-3 MB | ✅ Full symbols |
| Debug | ~5-10 MB | ✅ Full symbols |

For testing and profiling, the size increase is worth it!

## Performance Impact

**None!** Debug symbols don't affect runtime performance:

- ✅ Same optimizations as release build
- ✅ Same execution speed
- ✅ Same memory usage during execution
- ⚠️ Larger binary file on disk (one-time cost)

## Alternative: Separate Debug Files

If binary size is critical, you can keep debug info separate:

```bash
# Build with debug info
cargo build --release

# Extract debug info to separate file
objcopy --only-keep-debug \
  target/release/rust-workload \
  target/release/rust-workload.debug

# Strip the main binary
objcopy --strip-debug target/release/rust-workload

# Link them (profiling tools will find .debug file)
objcopy --add-gnu-debuglink=target/release/rust-workload.debug \
  target/release/rust-workload
```

This gives you:
- Small production binary
- Separate debug file for profiling

## Comparison with Other Languages

| Language | Debug Symbols by Default |
|----------|--------------------------|
| **Go** | ✅ Yes (always included) |
| **Rust (debug)** | ✅ Yes |
| **Rust (release)** | ❌ No (must enable) |
| **C/C++ (no flags)** | ❌ No |
| **C/C++ (-g)** | ✅ Yes |
| **Python** | ✅ Yes (interpreter symbols) |

## Integration Tests

The Rust test workload is now configured with debug symbols, so integration tests will show proper function names:

**Before:**
```
Found symbol: e400007f3537ae36: <no-symbol>
```

**After:**
```
Found symbol: rust_workload::main
Found symbol: std::thread::spawn
Found symbol: num_cpus::get
```

## Manual Testing

### Build and Profile

```bash
# Workload is already built with debug symbols
cd test/workloads/rust

# Run it
./target/release/rust-workload 60 4 &
PID=$!

# Profile it
cd ../../..
sudo ./perf-agent --profile --pid $PID --duration 30s

# View results (should show Rust function names!)
go tool pprof -top profile.pb.gz
go tool pprof -web profile.pb.gz
```

### Expected Output

```
Showing nodes accounting for XXX, 100% of XXX total
      flat  flat%   sum%        cum   cum%
     XXX   XX%    XX%       XXX   XX%  rust_workload::cpu_intensive_work
     XXX   XX%    XX%       XXX   XX%  std::thread::spawn
     XXX   XX%    XX%       XXX   XX%  num_cpus::get
```

## Troubleshooting

### Still Seeing `<no-symbol>`?

1. **Check Cargo.toml** has `[profile.release]` section
2. **Rebuild** with `cargo clean && cargo build --release`
3. **Verify** with `file target/release/rust-workload | grep debug_info`
4. **Check** symbols with `nm target/release/rust-workload | grep main`

### Binary Too Large?

Use the separate debug file approach (see above).

### Symbols Showing Mangled Names?

That's normal for Rust. Tools like `rustfilt` can demangle them:

```bash
# Demangle Rust symbols
go tool pprof -top profile.pb.gz | rustfilt
```

Or use `c++filt`:
```bash
go tool pprof -top profile.pb.gz | c++filt
```

## Best Practices

1. **Always enable debug symbols for profiling builds**
2. **Use release optimizations** (don't use debug builds for profiling)
3. **Verify symbols** before profiling session
4. **Document** if production builds need symbols

## Summary

✅ **DO:**
- Enable `debug = true` in `[profile.release]`
- Verify binary has symbols before profiling
- Use release build (with debug info) for accurate performance

❌ **DON'T:**
- Profile stripped release binaries (no symbols)
- Use debug builds for performance testing (too slow)
- Assume all binaries have symbols by default

## References

- [Cargo Profiles](https://doc.rust-lang.org/cargo/reference/profiles.html)
- [Debugging Rust](https://doc.rust-lang.org/book/appendix-04-useful-development-tools.html)
- [Linux Perf with Rust](https://www.kernel.org/doc/html/latest/admin-guide/perf-security.html)
