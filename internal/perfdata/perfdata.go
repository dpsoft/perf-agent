package perfdata

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
)

// EventSpec describes the perf event the captured samples come from. Filled
// from internal/perfevent's auto-detect probe.
type EventSpec struct {
	Type         uint32 // PERF_TYPE_*
	Config       uint64 // event-type-specific
	SamplePeriod uint64 // period (or freq Hz when Frequency = true)
	Frequency    bool   // whether SamplePeriod is a frequency
}

// MetaInfo captures host-level facts the writer stamps into feature sections.
type MetaInfo struct {
	Hostname  string
	OSRelease string
	NumCPUs   uint32
}

// Writer writes a perf.data file in the kernel's standard format. Methods
// AddComm, AddMmap2, AddSample, AddFinishedRound, AddBuildID are append-only
// and not concurrency-safe — callers (perf-agent's CPU profiler) call them
// from a single goroutine. Close finalizes the file (writes feature sections,
// patches header offsets/sizes).
//
// If any append fails (e.g. ENOSPC), the first error is latched and
// subsequent Add* calls become no-ops; Close returns the latched error.
// This keeps pos in sync with bytes actually written and prevents the
// finalizer from patching the header with offsets that point past the
// real end of the data section.
type Writer struct {
	f       *os.File
	bw      *bufio.Writer
	pos     int64 // current byte offset in file
	dataBeg int64 // offset where data section begins
	spec    EventSpec
	meta    MetaInfo
	err     error // first append/flush error; sticky

	// data accumulated for feature-section emission at Close
	buildIDs []BuildIDEntry
}

// Open creates a new perf.data file at path and writes the file header,
// attr section, and attr_id table. The data section starts immediately
// after, and Add* calls append records into it. Close patches header
// offsets/sizes and emits feature sections.
func Open(path string, spec EventSpec, meta MetaInfo) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("perfdata: create %s: %w", path, err)
	}
	bw := bufio.NewWriter(f)

	// Write a placeholder file header (will be patched on Close).
	encodeFileHeader(bw, fileHeader{})

	// Write attr section: one perf_event_attr.
	flags := uint64(flagDisabled | flagSampleIDAll | flagInherit | flagMmap | flagComm | flagMmap2)
	if spec.Frequency {
		flags |= flagFreq
	}
	encodeEventAttr(bw, eventAttr{
		typ:          spec.Type,
		config:       spec.Config,
		samplePeriod: spec.SamplePeriod,
		sampleType:   sampleTypeIP | sampleTypeTID | sampleTypeTime | sampleTypeCPU | sampleTypePeriod | sampleTypeCallchain,
		flags:        flags,
		wakeupEvents: 1,
	})

	// Write attr_id table — one section pointing at no IDs (we have one
	// attr, no event ID array).
	writeUint64LE(bw, 0) // ids.offset
	writeUint64LE(bw, 0) // ids.size

	dataBeg := int64(fileHeaderSize + attrV8Size + 16)
	return &Writer{
		f:       f,
		bw:      bw,
		pos:     dataBeg,
		dataBeg: dataBeg,
		spec:    spec,
		meta:    meta,
	}, nil
}

// writeRecord encodes the record into a temporary buffer, writes it to bw,
// advances pos by the actual bytes written, and latches any error so all
// subsequent appends become no-ops. Caller passes the encoder; we centralize
// the error-handling and pos-tracking here.
func (w *Writer) writeRecord(encode func(*bytes.Buffer)) {
	if w.err != nil {
		return
	}
	var buf bytes.Buffer
	encode(&buf)
	n, err := w.bw.Write(buf.Bytes())
	w.pos += int64(n)
	if err != nil {
		w.err = err
	}
}

// AddComm appends a PERF_RECORD_COMM record.
func (w *Writer) AddComm(r CommRecord) {
	w.writeRecord(func(b *bytes.Buffer) { encodeComm(b, r) })
}

// AddMmap2 appends a PERF_RECORD_MMAP2 record.
func (w *Writer) AddMmap2(r Mmap2Record) {
	w.writeRecord(func(b *bytes.Buffer) { encodeMmap2(b, r) })
}

// AddSample appends a PERF_RECORD_SAMPLE record.
func (w *Writer) AddSample(r SampleRecord) {
	w.writeRecord(func(b *bytes.Buffer) { encodeSample(b, r) })
}

// AddFinishedRound appends a PERF_RECORD_FINISHED_ROUND marker.
func (w *Writer) AddFinishedRound() {
	w.writeRecord(func(b *bytes.Buffer) { encodeFinishedRound(b) })
}

// AddBuildID records a binary's build-id for emission in the
// HEADER_BUILD_ID feature section at Close.
func (w *Writer) AddBuildID(e BuildIDEntry) {
	w.buildIDs = append(w.buildIDs, e)
}

// Close finalizes the file: emits feature sections, builds the feature
// index table, patches the file header's offsets/sizes/feature bitmap,
// and closes the underlying file. If any earlier Add* call hit a write
// error, Close returns that latched error after attempting to close the
// file; the on-disk output is then not a valid perf.data and the caller
// should delete it.
func (w *Writer) Close() error {
	if w.err != nil {
		_ = w.f.Close()
		return fmt.Errorf("perfdata: append failed: %w", w.err)
	}
	dataEnd := w.pos
	dataSize := uint64(dataEnd - w.dataBeg)

	// Emit feature payloads, recording each (offset, size).
	type feat struct {
		bit  int
		body []byte
	}
	var feats []feat

	if len(w.buildIDs) > 0 {
		var buf bytes.Buffer
		encodeBuildIDFeature(&buf, w.buildIDs)
		feats = append(feats, feat{bit: featBuildID, body: buf.Bytes()})
	}
	if w.meta.Hostname != "" {
		var buf bytes.Buffer
		encodeStringFeature(&buf, w.meta.Hostname)
		feats = append(feats, feat{bit: featHostname, body: buf.Bytes()})
	}
	if w.meta.OSRelease != "" {
		var buf bytes.Buffer
		encodeStringFeature(&buf, w.meta.OSRelease)
		feats = append(feats, feat{bit: featOSRelease, body: buf.Bytes()})
	}
	if w.meta.NumCPUs > 0 {
		var buf bytes.Buffer
		encodeNRCPUSFeature(&buf, w.meta.NumCPUs, w.meta.NumCPUs)
		feats = append(feats, feat{bit: featNRCPUS, body: buf.Bytes()})
	}

	// Per the kernel format: after the data section, the feature index
	// table is appended (one section{} entry per set bit, in bit-number
	// order). The actual feature payloads follow the index table.
	indexTableSize := int64(len(feats) * 16) // 16 bytes per section{}
	indexTableBeg := dataEnd
	payloadBeg := indexTableBeg + indexTableSize

	var indexEntries []section
	cursor := uint64(payloadBeg)
	addsFeatures := uint64(0)
	for _, f := range feats {
		indexEntries = append(indexEntries, section{
			offset: cursor,
			size:   uint64(len(f.body)),
		})
		cursor += uint64(len(f.body))
		addsFeatures |= 1 << f.bit
	}

	// Write the index table.
	encodeFeatureIndexTable(w.bw, indexEntries)
	w.pos += indexTableSize

	// Write the feature payloads.
	for _, f := range feats {
		n, _ := w.bw.Write(f.body)
		w.pos += int64(n)
	}

	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("perfdata: flush: %w", err)
	}

	// Now patch the file header (seek to 0, rewrite).
	if _, err := w.f.Seek(0, 0); err != nil {
		return fmt.Errorf("perfdata: seek: %w", err)
	}
	patchedBuf := bufio.NewWriter(w.f)
	encodeFileHeader(patchedBuf, fileHeader{
		attrs:        section{offset: fileHeaderSize, size: attrV8Size},
		data:         section{offset: uint64(w.dataBeg), size: dataSize},
		eventTypes:   section{offset: 0, size: 0},
		addsFeatures: addsFeatures,
	})
	if err := patchedBuf.Flush(); err != nil {
		return fmt.Errorf("perfdata: header patch flush: %w", err)
	}
	return w.f.Close()
}
