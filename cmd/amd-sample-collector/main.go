package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
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
	defaultRealSource = "rocm-smi"
	defaultROCMSMI    = "rocm-smi"
	maxRealSpacing    = 100 * time.Millisecond
	maxRealPolls      = 32
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

type collectorConfig struct {
	mode          string
	realSource    string
	kernelName    string
	deviceID      string
	deviceName    string
	queueID       string
	sleepBeforeMS int
}

type rocmSMIMetrics struct {
	deviceID     string
	deviceName   string
	gpuUse       int
	powerWatts   int
	temperatureC int
	vramUsedPct  int
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

func collectionWindow() (int64, int64, int64, int64, time.Duration, error) {
	startNS, err := bootTimeNS()
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}

	duration, err := time.ParseDuration(envOrDefault("PERF_AGENT_GPU_DURATION", "140ms"))
	if err != nil {
		return 0, 0, 0, 0, 0, err
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

	return startNS, startNS + sample1OffsetNS, startNS + sample2OffsetNS, startNS + durationNS, duration, nil
}

func queryROCMSMI(path string) (rocmSMIMetrics, error) {
	cmd := exec.Command(path, "--showuse", "--showpower", "--showtemp", "--showmemuse", "--showid", "--showproductname", "--json")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return rocmSMIMetrics{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return rocmSMIMetrics{}, err
	}

	var cards map[string]map[string]string
	if err := json.Unmarshal(out, &cards); err != nil {
		return rocmSMIMetrics{}, err
	}
	if len(cards) == 0 {
		return rocmSMIMetrics{}, fmt.Errorf("no devices in rocm-smi output")
	}

	cardKeys := make([]string, 0, len(cards))
	for key := range cards {
		cardKeys = append(cardKeys, key)
	}
	sort.Strings(cardKeys)
	cardKey := cardKeys[0]
	card := cards[cardKey]

	metrics := rocmSMIMetrics{
		deviceName: strings.TrimSpace(card["Device Name"]),
	}
	if metrics.deviceName == "" || strings.EqualFold(metrics.deviceName, "N/A") {
		metrics.deviceName = ""
	}

	if gfxVersion := strings.TrimSpace(card["GFX Version"]); gfxVersion != "" {
		cardIndex := strings.TrimPrefix(cardKey, "card")
		if cardIndex == cardKey {
			cardIndex = "0"
		}
		metrics.deviceID = fmt.Sprintf("%s:%s", gfxVersion, cardIndex)
	}

	if gpuUse := strings.TrimSpace(card["GPU use (%)"]); gpuUse != "" {
		value, err := strconv.Atoi(gpuUse)
		if err != nil {
			return rocmSMIMetrics{}, fmt.Errorf("parse GPU use (%%): %w", err)
		}
		metrics.gpuUse = value
	}

	if power := strings.TrimSpace(card["Current Socket Graphics Package Power (W)"]); power != "" {
		value, err := strconv.ParseFloat(power, 64)
		if err != nil {
			return rocmSMIMetrics{}, fmt.Errorf("parse power: %w", err)
		}
		metrics.powerWatts = int(math.Round(value))
	}

	if temperature := strings.TrimSpace(card["Temperature (Sensor edge) (C)"]); temperature != "" {
		value, err := strconv.ParseFloat(temperature, 64)
		if err != nil {
			return rocmSMIMetrics{}, fmt.Errorf("parse temperature: %w", err)
		}
		metrics.temperatureC = int(math.Round(value))
	}

	if vramUsedPct := strings.TrimSpace(card["GPU Memory Allocated (VRAM%)"]); vramUsedPct != "" {
		value, err := strconv.Atoi(vramUsedPct)
		if err != nil {
			return rocmSMIMetrics{}, fmt.Errorf("parse VRAM used (%%): %w", err)
		}
		metrics.vramUsedPct = value
	}

	return metrics, nil
}

func emitRecords(cfg collectorConfig, sample1Reason string, sample1Weight int, sample2Reason string, sample2Weight int) error {
	startNS, sample1NS, sample2NS, endNS, _, err := collectionWindow()
	if err != nil {
		return err
	}

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
		DeviceID: cfg.deviceID,
		Name:     cfg.deviceName,
	}
	q := queue{
		Backend: "amdsample",
		Device:  dev,
		QueueID: cfg.queueID,
	}

	if err := writeJSONLine(execRecord{
		Kind: "exec",
		Execution: execution{
			Backend:   "amdsample",
			DeviceID:  cfg.deviceID,
			QueueID:   cfg.queueID,
			ContextID: contextID,
			ExecID:    execID,
		},
		Correlation: correlation{Backend: "amdsample", Value: execID},
		Queue:       q,
		KernelName:  cfg.kernelName,
		StartNS:     startNS,
		EndNS:       endNS,
	}); err != nil {
		return fmt.Errorf("write exec record: %w", err)
	}

	if err := writeJSONLine(sampleRecord{
		Kind:         "sample",
		Correlation:  correlation{Backend: "amdsample", Value: sample1ID},
		Device:       dev,
		TimeNS:       sample1NS,
		KernelName:   cfg.kernelName,
		StallReason:  sample1Reason,
		SampleWeight: sample1Weight,
	}); err != nil {
		return fmt.Errorf("write sample record: %w", err)
	}

	if err := writeJSONLine(sampleRecord{
		Kind:         "sample",
		Correlation:  correlation{Backend: "amdsample", Value: sample2ID},
		Device:       dev,
		TimeNS:       sample2NS,
		KernelName:   cfg.kernelName,
		StallReason:  sample2Reason,
		SampleWeight: sample2Weight,
	}); err != nil {
		return fmt.Errorf("write sample record: %w", err)
	}

	return nil
}

func runSynthetic(cfg collectorConfig) error {
	return emitRecords(cfg, "memory_wait", 11, "wave_barrier", 5)
}

func runReal(cfg collectorConfig) error {
	switch cfg.realSource {
	case "", defaultRealSource:
		// supported below
	default:
		return fmt.Errorf("unsupported amd sample real source: %s", cfg.realSource)
	}

	startNS, _, _, endNS, duration, err := collectionWindow()
	if err != nil {
		return err
	}

	path := envOrDefault("PERF_AGENT_ROCM_SMI_PATH", defaultROCMSMI)

	realSpacing := duration / 2
	if realSpacing <= 0 {
		realSpacing = time.Millisecond
	}
	if realSpacing > maxRealSpacing {
		realSpacing = maxRealSpacing
	}
	if pollEnv := os.Getenv("PERF_AGENT_AMD_SAMPLE_REAL_POLL_INTERVAL"); pollEnv != "" {
		parsed, err := time.ParseDuration(pollEnv)
		if err != nil {
			return fmt.Errorf("unsupported PERF_AGENT_AMD_SAMPLE_REAL_POLL_INTERVAL: %w", err)
		}
		if parsed <= 0 {
			return fmt.Errorf("PERF_AGENT_AMD_SAMPLE_REAL_POLL_INTERVAL must be positive")
		}
		realSpacing = parsed
	}

	pollCount := int(duration/realSpacing) + 1
	if pollCount < 2 {
		pollCount = 2
	}
	if pollCount > maxRealPolls {
		pollCount = maxRealPolls
	}

	metricsByPoll := make([]rocmSMIMetrics, 0, pollCount)
	for i := 0; i < pollCount; i++ {
		metrics, err := queryROCMSMI(path)
		if err != nil {
			return fmt.Errorf("rocm-smi query failed: %w", err)
		}
		metricsByPoll = append(metricsByPoll, metrics)
		if i < pollCount-1 {
			time.Sleep(realSpacing)
		}
	}

	if cfg.deviceID == defaultDeviceID {
		for _, metrics := range metricsByPoll {
			if metrics.deviceID != "" {
				cfg.deviceID = metrics.deviceID
				break
			}
		}
	}
	if cfg.deviceName == defaultDeviceName {
		for _, metrics := range metricsByPoll {
			if metrics.deviceName != "" {
				cfg.deviceName = metrics.deviceName
				break
			}
		}
	}

	contextID := "ctx0"
	execID := fmt.Sprintf("dispatch:%d", startNS)
	if hipPID := os.Getenv("PERF_AGENT_HIP_PID"); hipPID != "" {
		contextID = fmt.Sprintf("pid-%s", hipPID)
		execID = fmt.Sprintf("dispatch:%s:%d", hipPID, startNS)
	}

	dev := device{
		Backend:  "amdsample",
		DeviceID: cfg.deviceID,
		Name:     cfg.deviceName,
	}
	q := queue{
		Backend: "amdsample",
		Device:  dev,
		QueueID: cfg.queueID,
	}

	if err := writeJSONLine(execRecord{
		Kind: "exec",
		Execution: execution{
			Backend:   "amdsample",
			DeviceID:  cfg.deviceID,
			QueueID:   cfg.queueID,
			ContextID: contextID,
			ExecID:    execID,
		},
		Correlation: correlation{Backend: "amdsample", Value: execID},
		Queue:       q,
		KernelName:  cfg.kernelName,
		StartNS:     startNS,
		EndNS:       endNS,
	}); err != nil {
		return fmt.Errorf("write exec record: %w", err)
	}

	durationNS := duration.Nanoseconds()
	for i, metrics := range metricsByPoll {
		sampleTimeNS := startNS + (int64(i+1) * durationNS / int64(pollCount+1))
		if sampleTimeNS >= endNS {
			sampleTimeNS = endNS - 1
		}

		gpuSampleID := fmt.Sprintf("sample:gpu:%d:%d", i, sampleTimeNS)
		if err := writeJSONLine(sampleRecord{
			Kind:         "sample",
			Correlation:  correlation{Backend: "amdsample", Value: gpuSampleID},
			Device:       dev,
			TimeNS:       sampleTimeNS,
			KernelName:   cfg.kernelName,
			StallReason:  "hardware_gpu_use",
			SampleWeight: metrics.gpuUse,
		}); err != nil {
			return fmt.Errorf("write sample record: %w", err)
		}

		powerSampleID := fmt.Sprintf("sample:power:%d:%d", i, sampleTimeNS)
		if err := writeJSONLine(sampleRecord{
			Kind:         "sample",
			Correlation:  correlation{Backend: "amdsample", Value: powerSampleID},
			Device:       dev,
			TimeNS:       sampleTimeNS,
			KernelName:   cfg.kernelName,
			StallReason:  "hardware_socket_power_watts",
			SampleWeight: metrics.powerWatts,
		}); err != nil {
			return fmt.Errorf("write sample record: %w", err)
		}

		tempSampleID := fmt.Sprintf("sample:temp:%d:%d", i, sampleTimeNS)
		if err := writeJSONLine(sampleRecord{
			Kind:         "sample",
			Correlation:  correlation{Backend: "amdsample", Value: tempSampleID},
			Device:       dev,
			TimeNS:       sampleTimeNS,
			KernelName:   cfg.kernelName,
			StallReason:  "hardware_temperature_c",
			SampleWeight: metrics.temperatureC,
		}); err != nil {
			return fmt.Errorf("write sample record: %w", err)
		}

		vramSampleID := fmt.Sprintf("sample:vram:%d:%d", i, sampleTimeNS)
		if err := writeJSONLine(sampleRecord{
			Kind:         "sample",
			Correlation:  correlation{Backend: "amdsample", Value: vramSampleID},
			Device:       dev,
			TimeNS:       sampleTimeNS,
			KernelName:   cfg.kernelName,
			StallReason:  "hardware_vram_used_pct",
			SampleWeight: metrics.vramUsedPct,
		}); err != nil {
			return fmt.Errorf("write sample record: %w", err)
		}
	}

	return nil
}

func main() {
	mode := flag.String("mode", envOrDefault("PERF_AGENT_AMD_SAMPLE_MODE", defaultMode), "collector mode (synthetic|real)")
	realSource := flag.String("real-source", envOrDefault("PERF_AGENT_AMD_SAMPLE_REAL_SOURCE", defaultRealSource), "real collector source")
	kernelName := flag.String("kernel-name", envOrDefault("PERF_AGENT_GPU_KERNEL_NAME", defaultKernelName), "kernel name to emit")
	deviceID := flag.String("device-id", envOrDefault("PERF_AGENT_GPU_DEVICE_ID", defaultDeviceID), "device id to emit")
	deviceName := flag.String("device-name", envOrDefault("PERF_AGENT_GPU_DEVICE_NAME", defaultDeviceName), "device name to emit")
	queueID := flag.String("queue-id", envOrDefault("PERF_AGENT_GPU_QUEUE_ID", defaultQueueID), "queue id to emit")
	sleepBeforeMS := flag.Int("sleep-before-ms", 250, "sleep before emitting samples, in milliseconds")
	flag.Parse()

	cfg := collectorConfig{
		mode:          *mode,
		realSource:    *realSource,
		kernelName:    *kernelName,
		deviceID:      *deviceID,
		deviceName:    *deviceName,
		queueID:       *queueID,
		sleepBeforeMS: *sleepBeforeMS,
	}

	sleepBefore(cfg.sleepBeforeMS)

	var err error
	switch cfg.mode {
	case "synthetic":
		err = runSynthetic(cfg)
	case "real":
		err = runReal(cfg)
	default:
		fmt.Fprintf(os.Stderr, "unsupported amd sample mode: %s\n", cfg.mode)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
