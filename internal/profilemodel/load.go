package profilemodel

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/dpsoft/perf-agent/internal/capturemeta"
)

// Digest is the SHA-256 of a profile's bytes as read.
type Digest [32]byte

// String returns the digest as lowercase hex.
func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// Availability says whether a fact source can be used and, if not, why.
type Availability struct {
	OK     bool
	Reason string
}

// Reasons an executions log is not used.
const (
	ReasonNoSidecar        = "no sidecar"
	ReasonEmptySidecar     = "empty sidecar"
	ReasonSidecarLimits    = "sidecar exceeds limits"
	ReasonDigestMismatch   = "digest mismatch"
	ReasonSampleCount      = "sample count mismatch"
	ReasonMetadataMismatch = "metadata mismatch"
	ReasonValueMismatch    = "value mismatch"
	ReasonCorrupt          = "corrupt"
)

// Limits bound what Load accepts.
type Limits struct {
	// MaxRawBytes caps the file; MaxDecompressedBytes caps the inflated
	// stream. Both are enforced before and during parsing.
	MaxRawBytes, MaxDecompressedBytes int64
	// MaxSamples, MaxLocations and MaxDepth are structural and checked after
	// parsing, before any tree is built. MaxDepth applies to the number of
	// locations in one sample.
	MaxSamples, MaxLocations, MaxDepth int
	// MaxSidecarBytes is enforced while reading the log.
	MaxSidecarBytes int64
	// MaxSidecarRows and MaxSidecarExecutions (distinct ordinals) are
	// enforced in the row callback.
	MaxSidecarRows, MaxSidecarExecutions int
}

// DefaultLimits returns the limits documented in the package comment.
func DefaultLimits() Limits {
	return Limits{
		MaxRawBytes:          256 << 20,
		MaxDecompressedBytes: 1 << 30,
		MaxSamples:           5_000_000,
		MaxLocations:         5_000_000,
		MaxDepth:             8192,
		MaxSidecarBytes:      256 << 20,
		MaxSidecarRows:       10_000_000,
		MaxSidecarExecutions: 10_000_000,
	}
}

// ErrLimit is wrapped by every error caused by an input exceeding a limit.
var ErrLimit = errors.New("profilemodel: input exceeds limit")

func limitErr(name string, limit, got any) error {
	return fmt.Errorf("%w: %s %v exceeded (%v)", ErrLimit, name, limit, got)
}

// Handle names a loaded profile: "<profile digest hex>", or
// "<profile digest hex>+<execlog sha hex>" when a log was accepted.
type Handle string

// Profile is a parsed pprof profile and, when one was accepted, the
// per-sample execution ordinals of its executions log.
type Profile struct {
	Handle     Handle
	Digest     Digest
	CaptureID  string // from "perf-agent.capture_id=", "" if absent
	Source     *profile.Profile
	Executions Availability
	execOf     [][]uint64 // per sample index: execution ordinals; nil unless Executions.OK
}

// Load reads a profile and, if execlogPath is not "", its executions log.
// A bad log never fails Load; it sets Executions.
func Load(profilePath, execlogPath string, lim Limits) (*Profile, error) {
	raw, err := readCapped(profilePath, lim.MaxRawBytes, "MaxRawBytes")
	if err != nil {
		return nil, err
	}
	var side []byte
	if execlogPath != "" {
		side, err = readCapped(execlogPath, lim.MaxSidecarBytes, "MaxSidecarBytes")
		switch {
		case errors.Is(err, ErrLimit):
			p, perr := FromBytes(raw, nil, lim)
			if perr != nil {
				return nil, perr
			}
			p.Executions = Availability{Reason: ReasonSidecarLimits}
			return p, nil
		case err != nil:
			p, perr := FromBytes(raw, nil, lim)
			if perr != nil {
				return nil, perr
			}
			p.Executions = Availability{Reason: ReasonCorrupt}
			return p, nil
		}
		if side == nil {
			side = []byte{}
		}
	}
	return FromBytes(raw, side, lim)
}

// readCapped reads a whole file, refusing one larger than max.
func readCapped(path string, limit int64, name string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, limitErr(name, limit, ">"+fmt.Sprint(limit))
	}
	return b, nil
}

// FromBytes is Load on bytes already in memory; execlog nil means none.
func FromBytes(raw []byte, execlogBytes []byte, lim Limits) (*Profile, error) {
	if int64(len(raw)) > lim.MaxRawBytes {
		return nil, limitErr("MaxRawBytes", lim.MaxRawBytes, len(raw))
	}
	data := raw
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("profilemodel: gzip: %w", err)
		}
		data, err = io.ReadAll(io.LimitReader(zr, lim.MaxDecompressedBytes+1))
		if err != nil {
			return nil, fmt.Errorf("profilemodel: gzip: %w", err)
		}
	}
	if int64(len(data)) > lim.MaxDecompressedBytes {
		return nil, limitErr("MaxDecompressedBytes", lim.MaxDecompressedBytes, ">"+fmt.Sprint(lim.MaxDecompressedBytes))
	}
	// ParseUncompressed never inflates again, so the cap above is the only
	// decompression bound (ParseData would re-inflate a nested gzip stream
	// without a limit).
	src, err := profile.ParseUncompressed(data)
	if err == nil {
		err = src.CheckValid()
	}
	if err != nil {
		return nil, fmt.Errorf("profilemodel: parse profile: %w", err)
	}
	if err := checkStructure(src, lim); err != nil {
		return nil, err
	}

	p := &Profile{
		Digest:     sha256.Sum256(raw),
		Source:     src,
		CaptureID:  captureID(src),
		Executions: Availability{Reason: ReasonNoSidecar},
	}
	p.Handle = Handle(p.Digest.String())

	if execlogBytes != nil {
		execOf, sha, reason := checkExeclog(raw, execlogBytes, p, lim)
		if reason != "" {
			p.Executions = Availability{Reason: reason}
		} else {
			p.Executions = Availability{OK: true}
			p.execOf = execOf
			p.Handle = Handle(p.Digest.String() + "+" + hex.EncodeToString(sha[:]))
		}
	}
	return p, nil
}

func checkStructure(p *profile.Profile, lim Limits) error {
	if len(p.Sample) > lim.MaxSamples {
		return limitErr("MaxSamples", lim.MaxSamples, len(p.Sample))
	}
	if len(p.Location) > lim.MaxLocations {
		return limitErr("MaxLocations", lim.MaxLocations, len(p.Location))
	}
	for _, s := range p.Sample {
		if len(s.Location) > lim.MaxDepth {
			return limitErr("MaxDepth", lim.MaxDepth, len(s.Location))
		}
	}
	return nil
}

func captureID(p *profile.Profile) string {
	for _, c := range p.Comments {
		if id, ok := strings.CutPrefix(c, capturemeta.CaptureIDComment); ok {
			return id
		}
	}
	return ""
}

// errSidecarLimit marks a row-callback stop caused by a sidecar limit.
var errSidecarLimit = errors.New("sidecar limit")

// errRowRange marks a row naming a sample the profile does not have.
var errRowRange = errors.New("row outside the profile")

// checkExeclog validates a log against the profile. It returns the per-sample
// ordinals and the log's digest on success, or the reason it is not used.
func checkExeclog(raw, log []byte, p *Profile, lim Limits) ([][]uint64, [32]byte, string) {
	var zero [32]byte
	if len(log) == 0 {
		return nil, zero, ReasonEmptySidecar
	}
	if int64(len(log)) > lim.MaxSidecarBytes {
		return nil, zero, ReasonSidecarLimits
	}
	n := len(p.Source.Sample)
	type acc struct {
		sum   uint64
		execs []uint64
	}
	bySample := map[uint32]*acc{}
	var nrows, nexecs int
	var last uint64
	seen := false
	hdr, tr, err := execlog.Read(bytes.NewReader(log), func(r execlog.Row) error {
		if int64(r.Sample) >= int64(n) {
			return errRowRange
		}
		nrows++
		if nrows > lim.MaxSidecarRows {
			return errSidecarLimit
		}
		if !seen || r.Execution != last {
			nexecs++
			if nexecs > lim.MaxSidecarExecutions {
				return errSidecarLimit
			}
			seen, last = true, r.Execution
		}
		a := bySample[r.Sample]
		if a == nil {
			a = &acc{}
			bySample[r.Sample] = a
		}
		sum := a.sum + r.Value
		if sum < a.sum {
			// Wrapped; remember it as an unmatchable sum.
			sum = ^uint64(0)
			a.sum = sum
			a.execs = append(a.execs, r.Execution)
			return nil
		}
		a.sum = sum
		if k := len(a.execs); k == 0 || a.execs[k-1] != r.Execution {
			a.execs = append(a.execs, r.Execution)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errSidecarLimit) {
			return nil, zero, ReasonSidecarLimits
		}
		return nil, zero, ReasonCorrupt
	}
	if execlog.ProfileSHA256(raw) != tr.ProfileSHA256 {
		return nil, zero, ReasonDigestMismatch
	}
	if err := tr.Verify(raw, n); err != nil {
		return nil, zero, ReasonSampleCount
	}

	gpu := -1
	for i, st := range p.Source.SampleType {
		if st.Type == "gpu" {
			gpu = i
			break
		}
	}
	if gpu < 0 {
		return nil, zero, ReasonMetadataMismatch
	}
	st := p.Source.SampleType[gpu]
	if hdr.SampleType != st.Type+"/"+st.Unit {
		return nil, zero, ReasonMetadataMismatch
	}
	if hdr.CaptureID != "" && p.CaptureID != "" && hdr.CaptureID != p.CaptureID {
		return nil, zero, ReasonMetadataMismatch
	}

	execOf := make([][]uint64, n)
	for i, s := range p.Source.Sample {
		if gpu >= len(s.Value) || s.Value[gpu] < 0 {
			return nil, zero, ReasonValueMismatch
		}
		var sum uint64
		if a := bySample[uint32(i)]; a != nil {
			sum = a.sum
			execOf[i] = a.execs
		}
		if sum != uint64(s.Value[gpu]) {
			return nil, zero, ReasonValueMismatch
		}
	}
	return execOf, sha256.Sum256(log), ""
}
