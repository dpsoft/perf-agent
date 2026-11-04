// Package execlog stores the per-execution detail of an aggregated GPU
// profile: one row per projected sample, naming the aggregated pprof sample it
// was summed into, the execution it came from, its correlation id and its
// value. It lets the GPU profile merge executions by stack and labels, which
// keeps its memory proportional to distinct stacks rather than to kernels,
// without losing which execution contributed what.
//
// A row is a projected sample, not an execution: with PC sampling one
// execution yields several rows. The execution ordinal ties them back
// together, so the number of executions is recoverable.
//
// File layout, v1 (all integers are varints unless noted):
//
//	magic "PAGPUEX1"
//	header: capture_id, backend, sample_type (length-prefixed strings)
//	rows:   0x01, sample_index, execution_delta, flags, [correlation_delta], value
//	trailer: 0xFF, rows, executions, aggregated_samples,
//	         profile SHA-256 (32 bytes), CRC-32 IEEE of everything before it (4 bytes, LE)
//
// execution_delta is the execution ordinal minus the previous row's ordinal,
// plus one on the first row; 0 means "same execution as the previous row".
// correlation_delta is the difference from the previous present correlation,
// taken modulo 2^64 and zigzag-encoded, so any uint64 sequence round-trips
// exactly, wrapping included. A file without a valid trailer is truncated or
// corrupt and is rejected whole.
package execlog

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

const (
	magic      = "PAGPUEX1"
	tagRow     = 0x01
	tagTrailer = 0xFF

	flagCorrelation = 1 << 0

	maxStringLen = 4096
)

// Header identifies what a log belongs to.
type Header struct {
	CaptureID  string
	Backend    string
	SampleType string
}

// Row is one projected sample.
type Row struct {
	// Sample is the index of the aggregated sample in the profile's final
	// serialized order.
	Sample uint32
	// Execution is the execution ordinal, starting at 0. Rows of one
	// execution are contiguous.
	Execution      uint64
	HasCorrelation bool
	Correlation    uint64
	// Value is what this row added to the aggregated sample, in SampleType
	// units. It can be 0 (a PC sample that received no weight).
	Value uint64
}

// Trailer closes a log.
type Trailer struct {
	Rows              uint64
	Executions        uint64
	AggregatedSamples uint64
	ProfileSHA256     [32]byte
}

// Writer streams rows to a temporary file beside the final path and publishes
// it with a rename only when Finish succeeds. Memory is O(1) in the number of
// rows. Errors are sticky: after the first one every call is a no-op and
// Finish removes the temporary file, never a file already at path.
type Writer struct {
	path, tmp string
	f         *os.File
	bw        *bufio.Writer
	crc       hash.Hash32
	err       error

	rows, executions uint64
	nextOrdinal      uint64 // previous ordinal + 1; 0 before the first row
	prevCorrelation  uint64
	done             bool
}

// Create starts a log for path. The temporary file is created exclusively in
// path's directory, so it is this capture's own and the final rename is
// atomic on the same filesystem.
func Create(path string, h Header) (*Writer, error) {
	for _, s := range []string{h.CaptureID, h.Backend, h.SampleType} {
		if len(s) > maxStringLen {
			return nil, errors.New("execlog: header field too long")
		}
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("execlog: create %s: %w", tmp, err)
	}
	w := &Writer{path: path, tmp: tmp, f: f, crc: crc32.NewIEEE()}
	w.bw = bufio.NewWriterSize(io.MultiWriter(f, w.crc), 256<<10)
	w.writeRaw([]byte(magic))
	for _, s := range []string{h.CaptureID, h.Backend, h.SampleType} {
		w.uvarint(uint64(len(s)))
		w.writeRaw([]byte(s))
	}
	if w.err != nil {
		w.Abort()
		return nil, w.err
	}
	return w, nil
}

func (w *Writer) writeRaw(b []byte) {
	if w.err != nil {
		return
	}
	_, w.err = w.bw.Write(b)
}

func (w *Writer) uvarint(v uint64) {
	var buf [binary.MaxVarintLen64]byte
	w.writeRaw(buf[:binary.PutUvarint(buf[:], v)])
}

// Add appends one row. Ordinals must not decrease; a decrease is a caller bug
// and makes the log invalid.
func (w *Writer) Add(r Row) {
	if w.err != nil || w.done {
		return
	}
	if r.Execution+1 < w.nextOrdinal || r.Execution == ^uint64(0) {
		w.err = fmt.Errorf("execlog: execution ordinal %d after %d", r.Execution, w.nextOrdinal-1)
		return
	}
	// nextOrdinal starts at 0, so the first row's delta is ordinal+1 >= 1.
	delta := r.Execution + 1 - w.nextOrdinal
	w.writeRaw([]byte{tagRow})
	w.uvarint(uint64(r.Sample))
	w.uvarint(delta)
	var flags byte
	if r.HasCorrelation {
		flags |= flagCorrelation
	}
	w.writeRaw([]byte{flags})
	if r.HasCorrelation {
		w.uvarint(zigzag(r.Correlation - w.prevCorrelation))
		w.prevCorrelation = r.Correlation
	}
	w.uvarint(r.Value)
	if delta > 0 {
		w.executions++
	}
	w.nextOrdinal = r.Execution + 1
	w.rows++
}

// Err reports the first error, if any.
func (w *Writer) Err() error { return w.err }

// Finish writes the trailer, syncs, closes and renames the log into place.
// On any error the temporary file is removed and nothing is published.
func (w *Writer) Finish(aggregatedSamples uint64, profileSHA256 [32]byte) error {
	if w.done {
		return errors.New("execlog: already finished")
	}
	w.done = true
	w.writeRaw([]byte{tagTrailer})
	w.uvarint(w.rows)
	w.uvarint(w.executions)
	w.uvarint(aggregatedSamples)
	w.writeRaw(profileSHA256[:])
	if w.err == nil {
		w.err = w.bw.Flush()
	}
	if w.err == nil {
		var sum [4]byte
		binary.LittleEndian.PutUint32(sum[:], w.crc.Sum32())
		_, w.err = w.f.Write(sum[:])
	}
	if w.err == nil {
		w.err = w.f.Sync()
	}
	if cerr := w.f.Close(); w.err == nil {
		w.err = cerr
	}
	if w.err == nil {
		w.err = os.Rename(w.tmp, w.path)
	}
	if w.err != nil {
		_ = os.Remove(w.tmp)
		return w.err
	}
	return nil
}

// Abort discards the log without publishing anything.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	_ = w.f.Close()
	_ = os.Remove(w.tmp)
}

// Rows and Executions report what has been added so far.
func (w *Writer) Rows() uint64       { return w.rows }
func (w *Writer) Executions() uint64 { return w.executions }

func zigzag(d uint64) uint64   { return (d << 1) ^ uint64(int64(d)>>63) }
func unzigzag(z uint64) uint64 { return (z >> 1) ^ -(z & 1) }

// ProfileSHA256 is the digest a log binds to: the profile's bytes exactly as
// written.
func ProfileSHA256(profile []byte) [32]byte { return sha256.Sum256(profile) }

// ErrCorrupt is returned for any malformed, truncated or inconsistent log.
var ErrCorrupt = errors.New("execlog: corrupt or truncated log")

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

type countingReader struct {
	r   *bufio.Reader
	crc hash.Hash32
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		_, _ = c.crc.Write([]byte{b})
	}
	return b, err
}

func (c *countingReader) full(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(c.r, b); err != nil {
		return nil, err
	}
	_, _ = c.crc.Write(b)
	return b, nil
}

// Read decodes a whole log, calling fn for each row in order. Rows passed to
// fn are only trustworthy if Read returns nil: the trailer, its counts and
// the checksum are verified after the last row. fn may return an error to
// stop early.
func Read(r io.Reader, fn func(Row) error) (Header, Trailer, error) {
	var h Header
	var t Trailer
	cr := &countingReader{r: bufio.NewReaderSize(r, 256<<10), crc: crc32.NewIEEE()}
	m, err := cr.full(len(magic))
	if err != nil || string(m) != magic {
		return h, t, corrupt("bad magic")
	}
	fields := []*string{&h.CaptureID, &h.Backend, &h.SampleType}
	for _, f := range fields {
		n, err := binary.ReadUvarint(cr)
		if err != nil || n > maxStringLen {
			return h, t, corrupt("bad header")
		}
		b, err := cr.full(int(n))
		if err != nil {
			return h, t, corrupt("bad header")
		}
		*f = string(b)
	}
	var rows, executions, nextOrdinal, prevCorrelation uint64
	var maxSample int64 = -1
	for {
		tag, err := cr.ReadByte()
		if err != nil {
			return h, t, corrupt("missing trailer")
		}
		if tag == tagTrailer {
			break
		}
		if tag != tagRow {
			return h, t, corrupt("unknown tag 0x%02x after %d rows", tag, rows)
		}
		var row Row
		sample, err := binary.ReadUvarint(cr)
		if err != nil || sample > uint64(^uint32(0)) {
			return h, t, corrupt("bad sample index at row %d", rows)
		}
		row.Sample = uint32(sample)
		delta, err := binary.ReadUvarint(cr)
		if err != nil || (rows == 0 && delta == 0) {
			return h, t, corrupt("bad execution delta at row %d", rows)
		}
		if delta > 0 {
			next := nextOrdinal + delta
			if next < nextOrdinal {
				return h, t, corrupt("execution ordinal overflow at row %d", rows)
			}
			nextOrdinal = next
			executions++
		}
		row.Execution = nextOrdinal - 1
		flags, err := cr.ReadByte()
		if err != nil || flags&^flagCorrelation != 0 {
			return h, t, corrupt("bad flags at row %d", rows)
		}
		if flags&flagCorrelation != 0 {
			z, err := binary.ReadUvarint(cr)
			if err != nil {
				return h, t, corrupt("bad correlation at row %d", rows)
			}
			row.HasCorrelation = true
			row.Correlation = prevCorrelation + unzigzag(z)
			prevCorrelation = row.Correlation
		}
		if row.Value, err = binary.ReadUvarint(cr); err != nil {
			return h, t, corrupt("bad value at row %d", rows)
		}
		if int64(row.Sample) > maxSample {
			maxSample = int64(row.Sample)
		}
		rows++
		if fn != nil {
			if err := fn(row); err != nil {
				return h, t, err
			}
		}
	}
	for _, f := range []*uint64{&t.Rows, &t.Executions, &t.AggregatedSamples} {
		if *f, err = binary.ReadUvarint(cr); err != nil {
			return h, t, corrupt("bad trailer")
		}
	}
	sha, err := cr.full(32)
	if err != nil {
		return h, t, corrupt("bad trailer")
	}
	copy(t.ProfileSHA256[:], sha)
	want := cr.crc.Sum32()
	var sum [4]byte
	if _, err := io.ReadFull(cr.r, sum[:]); err != nil || binary.LittleEndian.Uint32(sum[:]) != want {
		return h, t, corrupt("checksum mismatch")
	}
	if _, err := cr.r.ReadByte(); err != io.EOF {
		return h, t, corrupt("trailing bytes")
	}
	if t.Rows != rows || t.Executions != executions {
		return h, t, corrupt("trailer counts %d rows/%d executions, read %d/%d", t.Rows, t.Executions, rows, executions)
	}
	if maxSample >= int64(t.AggregatedSamples) {
		return h, t, corrupt("sample index %d out of range of %d aggregated samples", maxSample, t.AggregatedSamples)
	}
	return h, t, nil
}

// Verify checks that a log's trailer binds it to this exact profile.
func (t Trailer) Verify(profile []byte, profileSamples int) error {
	if ProfileSHA256(profile) != t.ProfileSHA256 {
		return errors.New("execlog: log belongs to a different profile (SHA-256 mismatch)")
	}
	if uint64(profileSamples) != t.AggregatedSamples {
		return fmt.Errorf("execlog: log expects %d aggregated samples, profile has %d", t.AggregatedSamples, profileSamples)
	}
	return nil
}
