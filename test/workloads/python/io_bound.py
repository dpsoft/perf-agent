#!/usr/bin/env python3
import os
import sys
import time
import threading

def io_work(duration, thread_id):
    """I/O-intensive operations"""
    end_time = time.time() + duration
    tmpfile = f"/tmp/perf-agent-test-py-{thread_id}.dat"

    try:
        while time.time() < end_time:
            # Write 1MB
            with open(tmpfile, 'wb') as f:
                f.write(b'0' * (1024 * 1024))
                f.flush()
                os.fsync(f.fileno())

            # Read it back
            with open(tmpfile, 'rb') as f:
                _ = f.read()

            time.sleep(0.1)
    finally:
        if os.path.exists(tmpfile):
            os.remove(tmpfile)

def warmup():
    """
    Warmup phase to force JIT compilation of all functions.
    This ensures the perf map is populated before profiling starts.
    """
    print(f"[Warmup] Starting JIT compilation warmup...", flush=True)
    start_time = time.time()
    
    # Call io_work with short duration to trigger compilation
    # Use 2 threads with unique IDs (100+) to avoid file conflicts
    threads = []
    for i in range(2):
        t = threading.Thread(target=io_work, args=(0.5, 100 + i))
        t.start()
        threads.append(t)
    
    for t in threads:
        t.join()
    
    elapsed = time.time() - start_time
    print(f"[Warmup] Completed in {elapsed:.2f}s - functions JIT-compiled", flush=True)
    
    # Extra sleep to ensure perf map is fully written
    time.sleep(0.5)

def main():
    duration = int(sys.argv[1]) if len(sys.argv) > 1 else 30
    num_threads = int(sys.argv[2]) if len(sys.argv) > 2 else 4

    print(f"Python I/O-bound workload: {num_threads} threads for {duration}s")
    print(f"PID: {os.getpid()}")
    
    # Run warmup to populate perf map
    warmup()
    
    print(f"[Main] Starting actual workload with {num_threads} threads...", flush=True)

    threads = []
    for i in range(num_threads):
        t = threading.Thread(target=io_work, args=(duration, i))
        t.start()
        threads.append(t)

    for t in threads:
        t.join()

    print("Python workload completed")

if __name__ == "__main__":
    main()
