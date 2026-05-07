# debuginfod integration test fixture

A minimal `debuginfod` server in Docker plus a sample C binary with separable
debug info, used by perf-agent's debuginfod integration tests.

## Layout

- `docker-compose.yml` — single-service `debuginfod` container on port 8002
- `sample/hello.c` + `sample/Makefile` — fixture binary built with
  `--build-id` and split debug info
- `upload.sh` — extracts `.debug` from the binary into the server's
  `.build-id` index
- `test.sh` — quick smoke test: GET /buildid/<X>/debuginfo and check sections

## Use

```bash
cd test/debuginfod
docker compose up -d debuginfod
# Build the sample binary (Linux ELF — use a Linux container if on macOS):
make -C sample
./upload.sh sample/hello
./test.sh sample/hello
```

The integration test in `test/debuginfod_integration_test.go`
(`TestSymbolizeViaDebuginfod`, lands in Task 23) boots the server, waits for
readiness, and runs the agent against the sample binary with
`--debuginfod-url=http://localhost:8002`.

## Cleanup

```bash
docker compose down -v
rm -rf debuginfo-store/.build-id
```
