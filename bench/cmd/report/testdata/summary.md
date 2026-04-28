# Scenario: `system-wide-mixed`

- **Started:** 2026-04-25 19:30:00 UTC
- **Kernel:** 6.19 · **CPU:** Test CPU · **NCPU:** 4 · **Go:** go1.26.0 · **Commit:** deadbeef
- **Config:** processes=30 runs=3 drop_cache=false

## Wall time (newSession startup)

| metric | value (ms) |
|--------|-----------|
| p50 | 1000.0 |
| p95 | 1100.0 |
| max | 1100.0 |

## Top binaries by median compile_ms

| binary | build_id | eh_frame_bytes | median compile_ms |
|--------|----------|----------------|-------------------|
| `/bin/foo` | `222222222222…` | 9000 | 50.00 |
| `/lib/libc.so` | `111111111111…` | 30000 | 10.00 |
