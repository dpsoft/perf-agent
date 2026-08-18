# USDT `.note.stapsdt` parser — report

## What was built

`internal/usdt` — a leaf package, pure Go (`debug/elf` only, no CGO, no new
dependencies), parsing `.note.stapsdt` notes out of an ELF file.

- `internal/usdt/usdt.go`: `Parse(io.ReaderAt) ([]Probe, error)` and
  `ParseFile(path string) ([]Probe, error)`. `Probe` carries `Provider`,
  `Name`, `Args` (raw, unparsed), `Offset` (file offset to attach a uprobe
  at), `HasSemaphore` + `SemaphoreOffset` (semaphore file offset, only
  meaningful when `HasSemaphore` is true — a semaphore offset of 0 is a
  legitimate file offset and is never confused with "no semaphore").
- Handles both ELF classes (32/64-bit pointer width in the note descriptor)
  and both byte orders, taken from the ELF header rather than assumed.
- Implements the two conversions called out in the task:
  - **vaddr → file offset**: scans `PT_LOAD` segments for one whose
    `[Vaddr, Vaddr+Filesz)` contains the address; an address in no segment
    is `ErrNoLoadSegment`, an address inside a segment's `Memsz` but beyond
    its `Filesz` (`.bss`) is `ErrInBSS`. Both are real, wrapped errors —
    never a silently wrong offset.
  - **Base/prelink adjustment**: when a `.stapsdt.base` section exists, every
    address in a note is shifted by `(.stapsdt.base's actual Addr) − (note's
    own base field)` before conversion. When the section is absent, no
    adjustment is applied.
- Multiple probes sharing a `(Provider, Name)` pair are never deduplicated —
  there's no map keyed on that pair anywhere in the code; probes accumulate
  in a plain slice in note order.
- A file with no `.note.stapsdt` section returns `(nil, nil)`, not an error.
- A malformed/truncated note (short header, descsz claiming more bytes than
  are present, an unterminated provider/name/args string) returns an error
  and **no** partial probe slice — `Parse` never returns `(probes, err)`
  with both non-nil.

Not done, per the task's explicit scope: no argument-descriptor parsing
(`Args` is stored verbatim), no wiring into `gpu/` or the agent.

`go vet` and `gofmt` are clean on the package.

## Fixtures and how to regenerate them

`internal/usdt/testdata/` commits three small (∼14 KB each) built binaries
plus their `.c` sources and a `gen.sh` regeneration script, following the
repo's existing convention of committing built artifacts (bpf2go `.o`
files):

- `probe` / `probe.c` — the inline-asm `.note.stapsdt` route (no systemtap
  dependency). Built at `-O2` **specifically** so gcc inlines `emit_batch`
  into `main` *and* keeps a standalone copy, duplicating the probe call site
  and producing **two** notes with identical `Provider`/`Name` and different
  `Location`/`Args`. `gen.sh` documents this and warns not to "simplify" the
  flag back to `-O0`, which silently collapses back to one probe and
  defeats the multi-probe test. No semaphore (`0x0`) in either note.
- `probe2` / `probe2.c` — `DTRACE_PROBE1` via `sys/sdt.h`, no
  `_SDT_HAS_SEMAPHORES`: one probe, `Semaphore: 0x0` — the "no semaphore"
  case.
- `probe4` / `probe4.c` — `STAP_PROBE1` with `_SDT_HAS_SEMAPHORES` and a
  self-declared semaphore variable: one probe with a real, nonzero
  semaphore address landing in the writable data segment.

`gen.sh` (`internal/usdt/testdata/gen.sh`) rebuilds all three with
`-no-pie -g` (plus the `-O2` override for `probe`) using `gcc` and
`sys/sdt.h` (Fedora: `dnf install systemtap-sdt-devel`). It is not required
at test time — tests read only the committed binaries — but running it after
editing the `.c` sources, or to refresh against a newer toolchain, is the
supported path. The task's original scratch fixtures at
`/tmp/.../scratchpad/usdt/` were **not** copied in; these binaries were
rebuilt from the same sources via `gen.sh` and their addresses independently
re-derived (see below) since that scratch directory is explicitly
disposable session state.

All three offsets/values asserted in the Go tests were derived by hand from
`readelf -n/-S/-l testdata/<fixture>` (note `Location`/`Base`/`Semaphore`,
section `.stapsdt.base` address, and `PT_LOAD` `Off`/`Vaddr`/`Filesz`), then
cross-checked against the raw byte at the computed file offset — every probe
location must be `0x90` (`nop`), the instruction every
`STAP_NOTE`/`DTRACE_PROBE`/`STAP_PROBE` call site compiles to. None of the
expected values in the tests were derived by running the code under test.

## Per-test mutation coverage

Black-box, against committed fixtures:

- **`TestParse_Probe_MultipleProbesSameName`** — asserts exactly two probes
  with distinct, readelf-derived offsets (`0x460`, `0x365`) and distinct
  `Args`. Catches any dedup keyed on `(Provider, Name)`. Verified live: added
  a `map[string]bool` dedup guard to `Parse` and this test failed (returned
  1 probe, wanted 2), then reverted.
- **`TestParse_Probe2_NoSemaphore`** — asserts `HasSemaphore == false` and
  a specific probe offset. Catches code that treats "semaphore field is
  0" as "semaphore is at file offset 0" instead of "absent". Verified live:
  changed the `if desc.semaphore != 0` guard to `if true`, and this test
  (plus the multi-probe test above, which also has no semaphore) failed with
  `address is not contained in any PT_LOAD segment` — address `0x0` isn't
  covered by any segment starting at `0x400000`, so the mutation surfaces as
  a loud error rather than a silently wrong `SemaphoreOffset: 0`.
- **`TestParse_Probe4_RealSemaphore`** — asserts `HasSemaphore == true`,
  the exact semaphore offset (`0x200c`, independently derived from the RW
  segment's `Off`/`Vaddr`/`Filesz`, not from this package's own arithmetic),
  and additionally opens the fixture with `debug/elf` directly and checks
  the resolved semaphore offset lands inside a `PF_W` segment — catching a
  bug that reuses the probe location's (executable) segment for the
  semaphore instead of resolving it independently.
- **`TestParse_ToolchainDrift`** — skipped if `gcc`/`sys/sdt.h` are
  unavailable; otherwise builds `probe2.c` fresh in a temp dir and compares
  `Provider`/`Name`/`Args`/`HasSemaphore` against the committed `probe2`
  parse (not exact `Offset`, since a different gcc could legitimately lay
  out `.text` differently), then independently confirms the fresh binary's
  own resolved offset is a real `nop`. Catches drift between the toolchain
  that produced `testdata/` and the one running the test.

Synthetic-ELF tests (hand-built via a minimal `debug/elf`-struct-based
builder in the test file, since the compiled fixtures can't exercise these):

- **`TestParse_NoStapsdtSection`** — well-formed ELF, no `.note.stapsdt`
  section. Catches treating "section absent" as an error instead of
  `(nil, nil)`.
- **`TestParse_MalformedNote_TruncatedHeader`** — note cut off inside the
  12-byte namesz/descsz/type header. Catches out-of-bounds reads (panic) or
  silently returning whatever was decoded before the cut as a complete,
  successful result.
- **`TestParse_MalformedNote_TruncatedDescriptor`** — header/name intact,
  descriptor bytes cut short of the declared `descsz`. Catches descriptor
  parsing that trusts `descsz` without checking it against the remaining
  buffer.
- **`TestParse_MalformedNote_UnterminatedString`** — addresses intact, the
  `name` string never hits a NUL within the descriptor. Catches
  string-scanning that runs past the buffer end or silently accepts a
  non-terminated tail.
- **`TestParse_BaseAdjustment_Applied`** — the load-bearing base-adjustment
  test. Two adjacent `PT_LOAD` segments; the note's raw location falls in
  the second segment, its base field matches that segment's start, but
  `.stapsdt.base`'s *actual* address is the first segment's start. Correct
  delta (`actual − note base` = `−0x1000`) lands the adjusted address in the
  *first* segment at a specific offset (`0x10`); "forget the delta" resolves
  into the second segment instead (wrong, different offset), and "flip the
  sign" resolves an address in neither segment (`ErrNoLoadSegment`). Both
  wrong implementations are distinguishable from correct, and from each
  other. Verified live: flipped the delta computation
  (`desc.base - baseSec.Addr` instead of `baseSec.Addr - desc.base`) and the
  test failed with exactly the predicted `ErrNoLoadSegment` on address
  `0x402010`.
- **`TestParse_BaseAdjustment_AbsentSection_NoShift`** — same "wrong" note
  base field, but no `.stapsdt.base` section present. Catches applying some
  other adjustment (e.g. against the lowest `PT_LOAD` `Vaddr`) instead of
  none when the section is genuinely absent.
- **`TestParse_32Bit`** — ELFCLASS32, 4-byte note addresses. Catches
  hardcoding 8-byte address reads regardless of class, which would misparse
  the whole descriptor layout (reading string bytes as address bytes).
- **`TestParse_64Bit_BigEndian`** — ELFDATA2MSB. Catches hardcoding
  `binary.LittleEndian` anywhere in the note-decoding path instead of using
  the byte order `elf.NewFile` detected from `EI_DATA`.
- **`TestParse_LocationInBSS`** — a location inside a segment's `Memsz` but
  beyond its `Filesz`. Asserts the error specifically wraps `ErrInBSS`
  (`errors.Is`). Catches a bounds check against `Memsz` instead of `Filesz`.
  Verified live: changed the bss-boundary check from `Filesz` to `Memsz` and
  the test failed (`Parse succeeded ... want ErrInBSS`).
- **`TestParse_LocationNotInAnySegment`** — address covered by no `PT_LOAD`
  segment. Asserts `errors.Is(err, ErrNoLoadSegment)`. Catches a loop that
  falls through to a zero-value or last-segment offset instead of erroring.
- **`TestParseNotes_MultipleConcatenated`** — two well-formed stapsdt notes
  plus an unrelated (non-`stapsdt`-owner, deliberately *unaligned* 5-byte
  descriptor) note interleaved before them. Catches padding/alignment
  arithmetic that's correct for exactly one note but drifts on the next, and
  owner/type filtering that doesn't cleanly skip unrelated notes.
- **`TestAlign4`** — table test including `align4(0xffffffff)`, asserting
  the result is `0x100000000` (not wrapped). Catches 32-bit-arithmetic
  overflow in the padding computation for a maliciously large
  namesz/descsz — the original draft of `align4` computed `n+3` in `uint32`
  before widening, which does wrap; this was caught in review before commit
  and fixed to widen to `uint64` before adding.

Four of the above mutations (dedup, semaphore-always-present, base-delta
sign flip, bss-boundary-check) were verified live during development by
temporarily mutating `usdt.go`, confirming the relevant test(s) failed with
the predicted symptom, and reverting — not just asserted in this report.

## Ambiguities / things worth flagging

- **The scratch fixtures at `/tmp/.../scratchpad/usdt/` were not used
  directly.** Per the task, that directory is disposable, so the committed
  `testdata/` binaries were rebuilt from the same `.c` sources via `gen.sh`
  on this box (same gcc 16.1.1, `systemtap-sdt-devel-5.5-1.fc44`). Their
  addresses differ slightly from the scratch originals (debug-info layout
  shifted `.text` a little for `probe2`/`probe4`; `probe`'s two locations
  matched exactly), but the structural properties the task calls for —
  duplicate-named probes in `probe`, zero semaphore in `probe2`, nonzero
  semaphore in `probe4` — are all present and independently reconfirmed via
  `readelf` and byte-level `nop` checks. If the reviewing agent expects the
  *exact* scratch-directory byte offsets, they won't match; I judged
  re-deriving fresh, independently-verified numbers from the actually-committed
  binaries to be more honest than hardcoding numbers from a binary that
  isn't in the repo.
- **No real binary on this box exercises a nonzero base/prelink delta.**
  Modern gcc/binutils don't prelink, so every compiled fixture has
  `.stapsdt.base`'s address exactly equal to the note's base field (delta
  zero) — confirmed by inspecting all three fixtures. This is exactly the
  gap the task anticipated ("Getting this wrong yields probes that attach
  at plausible-looking but incorrect offsets, so make it explicit and test
  it"), so `TestParse_BaseAdjustment_Applied` and
  `TestParse_BaseAdjustment_AbsentSection_NoShift` construct synthetic ELF
  files by hand rather than relying on a compiled fixture. I'm confident in
  the adjustment formula itself — it matches the task's description exactly
  and the bcc/libbpf convention it references — but it is worth a reviewer
  double-checking the synthetic test's arithmetic in the comment block above
  `TestParse_BaseAdjustment_Applied`, since that test is the only evidence
  for this behavior with no compiled/real-world binary backing it up.
- **`.superpowers/sdd/` did not exist in this worktree or repo** (checked
  the whole tree); created it to place this report, per the task's
  instruction to write it there.

## Status contract

- **Status:** DONE
- **Commit:** (see below — created after this report)
- **Test summary:** `go test ./internal/usdt/...` — 16/16 passed. `go vet`
  and `gofmt -l` clean.
- **Concerns:** the base-adjustment behavior has no compiled-binary backing
  (see ambiguities above) — synthetic-ELF test only, though the formula and
  test arithmetic were hand-verified and mutation-tested.
- **Report path:** `.superpowers/sdd/usdt-parser-report.md`
