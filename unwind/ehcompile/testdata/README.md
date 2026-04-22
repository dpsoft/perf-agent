# ehcompile test fixtures

## x86_64: hello / hello.golden

Trivial C binary for `TestCompile_GoldenFile_x86`.

Regenerate:
```
gcc -O0 -fno-omit-frame-pointer -o testdata/hello testdata/hello.c
GOTOOLCHAIN=auto go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_x86 -update
```

## arm64: hello_arm64 / hello_arm64.golden

Cross-compiled arm64 binary. Regenerate (requires aarch64-linux-gnu-gcc):
```
aarch64-linux-gnu-gcc -O0 -o testdata/hello_arm64 testdata/hello.c
GOTOOLCHAIN=auto go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_arm64 -update
```

If the arm64 toolchain isn't installed, the arm64 test skips cleanly.
