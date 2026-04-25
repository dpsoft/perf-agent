# ehcompile test fixtures

## x86_64: hello / hello.golden

Trivial C binary for `TestCompile_GoldenFile_x86`.

Regenerate:
```
gcc -O0 -fno-omit-frame-pointer -o testdata/hello testdata/hello.c
GOTOOLCHAIN=auto go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_x86 -update
```

## arm64: hello_arm64.o / hello_arm64.golden

A cross-compiled relocatable ELF *object file* (not a linked binary).
We build to `.o` rather than a full executable because the arm64
cross-compiler on most hosts lacks the target libc / crt*.o needed for
linking. The object file still contains `.eh_frame` and is a valid ELF
parseable by `debug/elf`, so Compile() works on it. PC values are
link-time placeholders; the snapshot captures CFI structure, not
absolute addresses.

Regenerate (requires aarch64-linux-gnu-gcc):
```
aarch64-linux-gnu-gcc -c -O0 -fno-omit-frame-pointer -o testdata/hello_arm64.o testdata/hello.c
GOTOOLCHAIN=auto go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_arm64 -update
```

If aarch64-linux-gnu-gcc is missing, install via `sudo dnf install gcc-aarch64-linux-gnu`
(Fedora) or `sudo apt install gcc-aarch64-linux-gnu` (Debian/Ubuntu). The test
skips cleanly if the fixture is absent.
