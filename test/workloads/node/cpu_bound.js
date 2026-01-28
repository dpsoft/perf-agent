#!/usr/bin/env node

// Simple CPU-bound Node.js workload for perf-agent testing.
// Usage:
//   node --perf-basic-prof cpu_bound.js <duration_sec> <threads>
//
// Example:
//   node --perf-basic-prof cpu_bound.js 20 4

const os = require("os");

function cpuWork(durationSec) {
  const end = Date.now() + durationSec * 1000;
  let x = 0;
  while (Date.now() < end) {
    // Busy loop with some math to keep JIT active.
    x += Math.sqrt(Math.random()) * Math.sin(x);
    if (!Number.isFinite(x)) {
      x = 0;
    }
  }
  return x;
}

async function main() {
  const duration = parseInt(process.argv[2] || "20", 10);
  const threads = parseInt(process.argv[3] || String(os.cpus().length), 10);

  console.log(`Node.js CPU-bound workload: ${threads} workers for ${duration}s`);
  console.log(`PID: ${process.pid}`);

  const workers = [];
  for (let i = 0; i < threads; i++) {
    workers.push(
      new Promise((resolve) => {
        setImmediate(() => {
          resolve(cpuWork(duration));
        });
      }),
    );
  }

  await Promise.all(workers);
  console.log("Node.js workload completed");
}

if (require.main === module) {
  main().catch((err) => {
    console.error("Workload failed:", err);
    process.exit(1);
  });
}

