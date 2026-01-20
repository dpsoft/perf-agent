# Python Profiling Guide

## Python Perf Support Requirements

### Python 3.12+ Required for Full Symbolization

Starting with Python 3.12, you need to use the `-X perf` flag to enable proper perf profiling support. Without this flag, profiles will only show system call symbols, not your Python function names.

## Quick Check

```bash
# Check if your Python supports perf profiling
bash test/check_python_perf.sh
```

## The Problem

**Without `-X perf` flag:**
```
Profile shows:
  - __clone3
  - pthread_create
  - _PyEval_EvalFrameDefault
  - (generic C/system symbols)
```

**With `-X perf` flag:**
```
Profile shows:
  - cpu_work (cpu_bound.py:9)
  - main (cpu_bound.py:20)
  - Your actual Python function names!
```

## How It Works

When you run Python with `-X perf`:

1. Python creates a perf map file: `/tmp/perf-<PID>.map`
2. This file maps memory addresses to Python function names
3. Perf tools (including perf-agent) read this file to symbolize stack traces
4. You get meaningful Python function names in your profiles

## Usage

### Manual Testing

```bash
# Start Python workload WITH perf support
python3 -X perf test/workloads/python/cpu_bound.py 60 4 &
PID=$!

# Verify perf map was created
ls -lh /tmp/perf-$PID.map
cat /tmp/perf-$PID.map | head -5

# Profile it
sudo ./perf-agent --profile --pid $PID --duration 30s

# Analyze (you should see Python function names!)
go tool pprof -top profile.pb.gz
go tool pprof -web profile.pb.gz
```

### Automated Tests

The integration tests automatically use `-X perf`:

```go
// In test/integration_test.go
{
    Name:     "python-cpu",
    Binary:   "python3",
    Args:     []string{"-X", "perf", "./workloads/python/cpu_bound.py", "20", "4"},
    Language: "python",
}
```

## Verification

### Method 1: Check Perf Map File

```bash
# Run Python with perf support
python3 -X perf your_script.py &
PID=$!

# Check if perf map exists
ls /tmp/perf-$PID.map

# View the contents
cat /tmp/perf-$PID.map | head -10
```

Expected output:
```
7f8a2c000000 1234 py::cpu_work:/path/to/cpu_bound.py
7f8a2c001234 5678 py::main:/path/to/cpu_bound.py
...
```

### Method 2: Compare Profiles

```bash
# Test WITHOUT -X perf
python3 test/workloads/python/cpu_bound.py 30 4 &
PID_BAD=$!
sudo ./perf-agent --profile --pid $PID_BAD --duration 10s
mv profile.pb.gz profile-without-perf.pb.gz

# Test WITH -X perf
python3 -X perf test/workloads/python/cpu_bound.py 30 4 &
PID_GOOD=$!
sudo ./perf-agent --profile --pid $PID_GOOD --duration 10s
mv profile.pb.gz profile-with-perf.pb.gz

# Compare
echo "=== Without -X perf ==="
go tool pprof -top profile-without-perf.pb.gz | head -15

echo ""
echo "=== With -X perf ==="
go tool pprof -top profile-with-perf.pb.gz | head -15
```

### Method 3: Test Validation

The integration tests include automatic validation:

```bash
# Run tests - they will report Python perf map status
sudo make test-integration

# Look for these messages in output:
# ✓ Python perf map found at /tmp/perf-12345.map
# ✓ Found Python-specific symbol: cpu_work in cpu_bound.py
# ✓ Python symbolization working correctly
```

## Python Version Compatibility

| Python Version | -X perf Support | Symbolization |
|----------------|-----------------|---------------|
| < 3.12 | ✗ Not available | System symbols only |
| 3.12+ | ✓ Available | Full Python function names |
| 3.13+ | ✓ Available | Full Python function names |

### Check Your Python Version

```bash
python3 --version

# Or more detailed
python3 -c "import sys; print(f'Python {sys.version_info.major}.{sys.version_info.minor}.{sys.version_info.micro}')"
```

### If You Have Python < 3.12

**Option 1: Upgrade Python**
```bash
# Fedora/RHEL
sudo dnf install python3.12

# Ubuntu/Debian
sudo apt install python3.12

# Or use pyenv for multiple versions
pyenv install 3.12.0
pyenv global 3.12.0
```

**Option 2: Accept Limited Symbolization**

Profiles will still work and show:
- System call symbols
- CPU usage patterns
- Call stacks (but with C-level function names)
- Performance hotspots (at system level)

You just won't see your Python function names like `cpu_work()`.

## Integration Test Behavior

### With Python 3.12+

```
=== RUN   TestProfileMode/python-cpu
    ✓ Python perf map found at /tmp/perf-84567.map
    Sample entry: 7f8a2c000000 1234 py::cpu_work:cpu_bound.py
    Found symbol: cpu_work (file: cpu_bound.py)
    ✓ Found Python-specific symbol: cpu_work in cpu_bound.py
    ✓ Python symbolization working correctly
--- PASS: TestProfileMode/python-cpu (12.93s)
```

### With Python < 3.12

```
=== RUN   TestProfileMode/python-cpu
    ⚠ WARNING: Python perf map not found at /tmp/perf-84567.map
    Python version may not support -X perf (requires 3.12+)
    Found symbol: __clone3 (file: )
    ⚠ No Python-specific symbols found (may need Python 3.12+ with -X perf)
    Profile contains system symbols only, which is expected without perf map support
--- PASS: TestProfileMode/python-cpu (12.93s)
```

The test still passes, but with warnings about limited symbolization.

## Troubleshooting

### Perf Map Not Created

**Problem:** `/tmp/perf-<PID>.map` doesn't exist even with `-X perf`

**Solutions:**
1. Verify Python version: `python3 --version` (need 3.12+)
2. Check if flag is recognized: `python3 -X perf -c "print('test')"`
3. Ensure /tmp is writable: `touch /tmp/test && rm /tmp/test`
4. Try with longer-running script (perf map created after initialization)

### Profile Shows No Python Symbols

**Problem:** Profile has samples but no Python function names

**Checklist:**
- [ ] Python 3.12+ installed
- [ ] Used `-X perf` flag when starting Python
- [ ] Perf map file exists: `ls /tmp/perf-*.map`
- [ ] Perf map has entries: `cat /tmp/perf-<PID>.map`
- [ ] Profile was collected while Python was running
- [ ] Symbolization ran on same system (perf map is local)

### Permission Issues

**Problem:** Can't read perf map file

```bash
# Check permissions
ls -l /tmp/perf-*.map

# Fix if needed (run as same user)
sudo chown $USER /tmp/perf-*.map
```

## Best Practices

1. **Always use `-X perf` for Python profiling** (3.12+)
2. **Let Python initialize** before profiling (wait 1-2 seconds)
3. **Keep Python running** while collecting profile
4. **Profile on same system** where perf map was created
5. **Check perf map file** exists before profiling

## Examples

### Basic CPU Profiling

```bash
# Start Python with perf support
python3 -X perf -c "
import time
def hot_function():
    return sum(i**2 for i in range(1000000))

while True:
    hot_function()
    time.sleep(0.1)
" &
PID=$!

# Profile it
sudo ./perf-agent --profile --pid $PID --duration 30s

# View results
go tool pprof -top profile.pb.gz
# Should show: hot_function, <listcomp>, etc.

# Cleanup
kill $PID
```

### Multi-threaded Python Profiling

```bash
# Profile the test workload
python3 -X perf test/workloads/python/cpu_bound.py 60 4 &
PID=$!

sudo ./perf-agent --profile --pmu --pid $PID --duration 30s

# View profile
go tool pprof -top profile.pb.gz
# Should show: cpu_work, math.sqrt, math.sin, etc.

kill $PID
```

## References

- [Python Performance Profiling with perf](https://docs.python.org/3/howto/perf_profiling.html)
- [Python 3.12 Release Notes](https://docs.python.org/3/whatsnew/3.12.html#perf-profiling-support)
- [perf Wiki](https://perf.wiki.kernel.org/index.php/Main_Page)

## Known Issues

See [PYTHON_PERF_KNOWN_ISSUES.md](PYTHON_PERF_KNOWN_ISSUES.md) for:
- JIT compilation timing issues
- Why Python symbols may not appear immediately
- Workarounds and manual testing procedures

## Summary

✅ **DO:**
- Use Python 3.12+
- Add `-X perf` flag
- Allow warmup time (5-10 seconds) before profiling
- Verify perf map file exists and has your functions
- Check test output for Python symbol validation

⚠️ **DON'T:**
- Profile Python < 3.12 expecting function names
- Forget the `-X perf` flag
- Profile immediately after starting Python (wait for JIT)
- Assume profiles work without verification
- Try to use perf maps from different systems
