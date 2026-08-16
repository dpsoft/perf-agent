package perfdata

import "io"

// PERF_TYPE_* constants (uapi/linux/perf_event.h).
const (
	perfTypeHardware = 0
	perfTypeSoftware = 1
)

// PERF_COUNT_HW_* constants.
const (
	perfCountHWCPUCycles = 0
)

// PERF_COUNT_SW_* constants.
const (
	perfCountSWCPUClock = 0
)

// PERF_SAMPLE_* bits used in perf_event_attr.sample_type.
// Subset of what the kernel defines; we only emit what we use.
const (
	sampleTypeIP        = 1 << 0
	sampleTypeTID       = 1 << 1
	sampleTypeTime      = 1 << 2
	sampleTypeAddr      = 1 << 3
	sampleTypeRead      = 1 << 4
	sampleTypeCallchain = 1 << 5
	sampleTypeID        = 1 << 6
	sampleTypeCPU       = 1 << 7
	sampleTypePeriod    = 1 << 8
	sampleTypeStreamID  = 1 << 9
	sampleTypeRaw       = 1 << 10
)

// flag* bits packed into perf_event_attr's flags word. We define only the
// ones we set; the rest stay zero. Bit positions per uapi/linux/perf_event.h.
const (
	flagDisabled    = 1 << 0
	flagInherit     = 1 << 1
	flagPinned      = 1 << 2
	flagExclusive   = 1 << 3
	flagExcludeUser = 1 << 4
	flagExcludeKernel = 1 << 5
	flagExcludeHV   = 1 << 6
	flagExcludeIdle = 1 << 7
	flagMmap        = 1 << 8
	flagComm        = 1 << 9
	flagFreq        = 1 << 10
	flagSampleIDAll = 1 << 18 // critical: lets sample_id_all stamp every record
	flagMmap2       = 1 << 23
	flagCommExec    = 1 << 24
)

// eventAttr is the in-memory image of struct perf_event_attr v8. We only
// fill the fields we actually use; everything else stays zero. Total
// on-disk size is attrV8Size = 136 bytes.
type eventAttr struct {
	typ          uint32 // PERF_TYPE_*
	config       uint64 // event-type-specific
	samplePeriod uint64 // sample period, OR sample frequency if flagFreq is set
	sampleType   uint64 // bitmask of sampleType*
	flags        uint64 // bitmask of flag*
	wakeupEvents uint32 // wake user space when N samples buffered
}

// encodeEventAttr writes the 136-byte on-disk representation. Field order
// matches struct perf_event_attr in uapi/linux/perf_event.h.
func encodeEventAttr(w io.Writer, a eventAttr) {
	writeUint32LE(w, a.typ)                 // 0   type
	writeUint32LE(w, attrV8Size)            // 4   size
	writeUint64LE(w, a.config)              // 8   config
	writeUint64LE(w, a.samplePeriod)        // 16  sample_period (or sample_freq)
	writeUint64LE(w, a.sampleType)          // 24  sample_type
	writeUint64LE(w, 0)                     // 32  read_format (we don't read counters)
	writeUint64LE(w, a.flags)               // 40  flags bitfield
	writeUint32LE(w, a.wakeupEvents)        // 48  wakeup_events / wakeup_watermark
	writeUint32LE(w, 0)                     // 52  bp_type
	writeUint64LE(w, 0)                     // 56  bp_addr / config1
	writeUint64LE(w, 0)                     // 64  bp_len / config2
	writeUint64LE(w, 0)                     // 72  branch_sample_type (LBR — v2)
	writeUint64LE(w, 0)                     // 80  sample_regs_user
	writeUint32LE(w, 0)                     // 88  sample_stack_user
	writeUint32LE(w, 0)                     // 92  clockid (signed; 0 = use perf clock)
	writeUint64LE(w, 0)                     // 96  sample_regs_intr
	writeUint32LE(w, 0)                     // 104 aux_watermark
	writeUint16LE(w, 0)                     // 108 sample_max_stack
	writeUint16LE(w, 0)                     // 110 __reserved_2
	writeUint32LE(w, 0)                     // 112 aux_sample_size
	writeUint32LE(w, 0)                     // 116 __reserved_3
	writeUint64LE(w, 0)                     // 120 sig_data
	writeUint64LE(w, 0)                     // 128 config3 (reserved in v8)
}
