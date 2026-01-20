#!/usr/bin/env python3
import os
import sys
import time
import math
import threading
from multiprocessing import cpu_count

def cpu_work(duration, thread_id):
    """CPU-intensive computation"""
    end_time = time.time() + duration
    total = 0

    while time.time() < end_time:
        for i in range(10000):
            total += math.sqrt(i) * math.sin(i)

    return total

def warmup():
    """
    Warmup phase to force JIT compilation of all functions.
    This ensures the perf map is populated before profiling starts.
    """
    print(f"[Warmup] Starting JIT compilation warmup...", flush=True)
    start_time = time.time()
    
    # Call cpu_work with short duration to trigger compilation
    # Use 2 threads to ensure thread creation code is also compiled
    threads = []
    for i in range(2):
        t = threading.Thread(target=cpu_work, args=(0.5, i))
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
    num_threads = int(sys.argv[2]) if len(sys.argv) > 2 else cpu_count()

    print(f"Python CPU-bound workload: {num_threads} threads for {duration}s")
    print(f"PID: {os.getpid()}")
    
    # Run warmup to populate perf map
    warmup()
    
    print(f"[Main] Starting actual workload with {num_threads} threads...", flush=True)

    threads = []
    for i in range(num_threads):
        t = threading.Thread(target=cpu_work, args=(duration, i))
        t.start()
        threads.append(t)

    for t in threads:
        t.join()

    print("Python workload completed")

if __name__ == "__main__":
    main()
