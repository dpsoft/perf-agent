package perfreader

// Sample is one parsed PERF_RECORD_SAMPLE. Fields mirror the kernel's sample
// format in the order and subset this package requests via Sample_type:
// PERF_SAMPLE_IP | TID | TIME | CALLCHAIN | REGS_USER | STACK_USER.
//
// Any field the kernel skipped (e.g. REGS_USER when the sampled task was in
// kernel mode and no user regs were available) will be zero-valued; callers
// must check ABI / StackSize before trusting Regs / Stack.
type Sample struct {
	IP        uint64   // the sampled instruction pointer
	PID       uint32   // process ID (tgid) of the sampled task
	TID       uint32   // thread ID of the sampled task
	Time      uint64   // nanoseconds since boot, monotonic clock
	Callchain []uint64 // kernel-walked FP callchain; includes sentinels like PERF_CONTEXT_USER
	ABI       uint64   // PERF_SAMPLE_REGS_ABI_{NONE,32,64}
	Regs      []uint64 // captured user registers, order defined by SampleRegsUser
	StackAddr uint64   // RSP/SP at sample time — base address of Stack bytes
	Stack     []byte   // raw stack memory starting at StackAddr
}

// Lost indicates the kernel dropped N samples due to ring-buffer overflow.
// Emitted separately from Sample via the reader's event channel.
type Lost struct {
	ID   uint64
	Lost uint64
}
