package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultKernelName = "hip_launch_shim_kernel"
	defaultDeviceID   = "gfx1103:0"
	defaultDeviceName = "AMD Radeon 780M Graphics"
	defaultQueueID    = "compute:0"
	defaultMode       = "synthetic"
)

type correlation struct {
	Backend string `json:"backend"`
	Value   string `json:"value"`
}

type device struct {
	Backend  string `json:"backend"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
}

type queue struct {
	Backend string `json:"backend"`
	Device  device `json:"device"`
	QueueID string `json:"queue_id"`
}

type execution struct {
	Backend   string `json:"backend"`
	DeviceID  string `json:"device_id"`
	QueueID   string `json:"queue_id"`
	ContextID string `json:"context_id"`
	ExecID    string `json:"exec_id"`
}

type execRecord struct {
	Kind        string      `json:"kind"`
	Execution   execution   `json:"execution"`
	Correlation correlation `json:"correlation"`
	Queue       queue       `json:"queue"`
	KernelName  string      `json:"kernel_name"`
	StartNS     int64       `json:"start_ns"`
	EndNS       int64       `json:"end_ns"`
}

type sampleRecord struct {
	Kind         string      `json:"kind"`
	Correlation  correlation `json:"correlation"`
	Device       device      `json:"device"`
	TimeNS       int64       `json:"time_ns"`
	KernelName   string      `json:"kernel_name"`
	StallReason  string      `json:"stall_reason"`
	SampleWeight int         `json:"weight"`
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func bootTimeNS() (int64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("unexpected /proc/uptime contents")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(seconds * float64(time.Second)), nil
}

func sleepBefore(ms int) {
	if ms <= 0 {
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func writeJSONLine(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func main() {
	mode := flag.String("mode", envOrDefault("PERF_AGENT_AMD_SAMPLE_MODE", defaultMode), "collector mode (synthetic|real)")
	kernelName := flag.String("kernel-name", envOrDefault("PERF_AGENT_GPU_KERNEL_NAME", defaultKernelName), "kernel name to emit")
	deviceID := flag.String("device-id", envOrDefault("PERF_AGENT_GPU_DEVICE_ID", defaultDeviceID), "device id to emit")
	deviceName := flag.String("device-name", envOrDefault("PERF_AGENT_GPU_DEVICE_NAME", defaultDeviceName), "device name to emit")
	queueID := flag.String("queue-id", envOrDefault("PERF_AGENT_GPU_QUEUE_ID", defaultQueueID), "queue id to emit")
	sleepBeforeMS := flag.Int("sleep-before-ms", 250, "sleep before emitting samples, in milliseconds")
	flag.Parse()

	switch *mode {
	case "synthetic":
	case "real":
		fmt.Fprintln(os.Stderr, "real amd sample collection is not implemented")
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "unsupported amd sample mode: %s\n", *mode)
		os.Exit(1)
	}

	sleepBefore(*sleepBeforeMS)

	startNS, err := bootTimeNS()
	if err != nil {
		fmt.Fprintf(os.Stderr, "boot time: %v\n", err)
		os.Exit(1)
	}

	duration, err := time.ParseDuration(envOrDefault("PERF_AGENT_GPU_DURATION", "140ms"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "unsupported PERF_AGENT_GPU_DURATION: %v\n", err)
		os.Exit(1)
	}
	durationNS := duration.Nanoseconds()
	sample1OffsetNS := durationNS / 4
	sample2OffsetNS := (durationNS * 3) / 4
	if sample1OffsetNS <= 0 {
		sample1OffsetNS = 1
	}
	if sample2OffsetNS <= sample1OffsetNS {
		sample2OffsetNS = sample1OffsetNS + 1
	}
	if sample2OffsetNS >= durationNS {
		sample2OffsetNS = durationNS - 1
	}
	if sample2OffsetNS <= sample1OffsetNS {
		sample1OffsetNS = 1
		sample2OffsetNS = 2
		durationNS = 3
	}

	sample1NS := startNS + sample1OffsetNS
	sample2NS := startNS + sample2OffsetNS
	endNS := startNS + durationNS

	contextID := "ctx0"
	execID := fmt.Sprintf("dispatch:%d", startNS)
	if hipPID := os.Getenv("PERF_AGENT_HIP_PID"); hipPID != "" {
		contextID = fmt.Sprintf("pid-%s", hipPID)
		execID = fmt.Sprintf("dispatch:%s:%d", hipPID, startNS)
	}
	sample1ID := fmt.Sprintf("sample:%d", sample1NS)
	sample2ID := fmt.Sprintf("sample:%d", sample2NS)

	dev := device{
		Backend:  "amdsample",
		DeviceID: *deviceID,
		Name:     *deviceName,
	}
	q := queue{
		Backend: "amdsample",
		Device:  dev,
		QueueID: *queueID,
	}

	if err := writeJSONLine(execRecord{
		Kind: "exec",
		Execution: execution{
			Backend:   "amdsample",
			DeviceID:  *deviceID,
			QueueID:   *queueID,
			ContextID: contextID,
			ExecID:    execID,
		},
		Correlation: correlation{Backend: "amdsample", Value: execID},
		Queue:       q,
		KernelName:  *kernelName,
		StartNS:     startNS,
		EndNS:       endNS,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "write exec record: %v\n", err)
		os.Exit(1)
	}

	if err := writeJSONLine(sampleRecord{
		Kind:         "sample",
		Correlation:  correlation{Backend: "amdsample", Value: sample1ID},
		Device:       dev,
		TimeNS:       sample1NS,
		KernelName:   *kernelName,
		StallReason:  "memory_wait",
		SampleWeight: 11,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "write sample record: %v\n", err)
		os.Exit(1)
	}

	if err := writeJSONLine(sampleRecord{
		Kind:         "sample",
		Correlation:  correlation{Backend: "amdsample", Value: sample2ID},
		Device:       dev,
		TimeNS:       sample2NS,
		KernelName:   *kernelName,
		StallReason:  "wave_barrier",
		SampleWeight: 5,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "write sample record: %v\n", err)
		os.Exit(1)
	}
}
