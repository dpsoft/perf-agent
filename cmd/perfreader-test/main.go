// perfreader-test is a diagnostic for the perfreader package. Given a PID,
// it samples at 99 Hz, parses each PERF_RECORD_SAMPLE, and prints a summary
// per sample. Exists only to validate the kernel-side capture before the
// DWARF unwinder is wired up.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/unwind/perfreader"
)

func main() {
	pid := flag.Int("pid", 0, "target PID (required)")
	freq := flag.Uint64("freq", 99, "sample frequency (Hz)")
	stackBytes := flag.Uint("stack-bytes", 8192, "user-stack bytes to capture per sample")
	limit := flag.Int("limit", 20, "stop after printing this many samples")
	duration := flag.Int("duration", 10, "stop after this many seconds regardless of sample count")
	flag.Parse()

	if *pid == 0 {
		fmt.Fprintln(os.Stderr, "usage: perfreader-test --pid <PID> [--freq 99] [--stack-bytes 8192] [--limit 20]")
		os.Exit(2)
	}

	cfg := perfreader.DefaultConfig()
	cfg.PID = *pid
	cfg.CPU = -1
	cfg.SampleFreq = *freq
	cfg.StackBytes = uint32(*stackBytes)

	r, err := perfreader.NewReader(cfg)
	if err != nil {
		log.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	// epoll-wait on the perf fd so we block instead of spinning.
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		log.Fatalf("epoll_create1: %v", err)
	}
	defer unix.Close(epfd)

	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, r.FD(), &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(r.FD()),
	}); err != nil {
		log.Fatalf("epoll_ctl: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf("perfreader-test: pid=%d freq=%dHz stack=%dB limit=%d duration=%ds\n",
		*pid, *freq, *stackBytes, *limit, *duration)
	fmt.Printf("sampling… (Ctrl+C to stop early)\n\n")

	count := 0
	wakeups := 0
	totalRecords := 0
	events := make([]unix.EpollEvent, 4)
	deadline := time.Now().Add(time.Duration(*duration) * time.Second)
	lastProgress := time.Now()

loop:
	for count < *limit && time.Now().Before(deadline) {
		select {
		case <-sig:
			break loop
		default:
		}

		n, err := unix.EpollWait(epfd, events, 500)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			log.Fatalf("epoll_wait: %v", err)
		}
		if n > 0 {
			wakeups++
		}

		records, err := r.ReadNext(func(s perfreader.Sample) {
			count++
			if count <= *limit {
				printSample(count, s)
			}
		})
		if err != nil {
			log.Printf("ReadNext: %v", err)
			break
		}
		totalRecords += records

		// Print a progress line every 2 seconds so silent hangs are visible.
		if time.Since(lastProgress) > 2*time.Second {
			fmt.Fprintf(os.Stderr, "[%.0fs] wakeups=%d records=%d samples=%d\n",
				time.Since(deadline.Add(-time.Duration(*duration)*time.Second)).Seconds(),
				wakeups, totalRecords, count)
			lastProgress = time.Now()
		}
	}

	fmt.Printf("\n[done: %d samples printed, %d records total, %d wakeups]\n",
		count, totalRecords, wakeups)
}

func printSample(n int, s perfreader.Sample) {
	fmt.Printf("--- sample #%d ---\n", n)
	fmt.Printf("  pid/tid : %d/%d\n", s.PID, s.TID)
	fmt.Printf("  time    : %d ns\n", s.Time)
	fmt.Printf("  IP      : 0x%016x\n", s.IP)
	fmt.Printf("  callchain (%d frames):\n", len(s.Callchain))
	for i, pc := range s.Callchain {
		tag := ""
		// PERF_CONTEXT_* sentinels from include/uapi/linux/perf_event.h
		switch pc {
		case 0xffffffffffffffe0: // PERF_CONTEXT_HV = -32
			tag = " [hv]"
		case 0xffffffffffffff80: // PERF_CONTEXT_KERNEL = -128
			tag = " [kernel]"
		case 0xfffffffffffffe00: // PERF_CONTEXT_USER = -512
			tag = " [user]"
		}
		fmt.Printf("    [%02d] 0x%016x%s\n", i, pc, tag)
		if i >= 9 && len(s.Callchain) > 12 {
			fmt.Printf("    ... (%d more)\n", len(s.Callchain)-i-1)
			break
		}
	}
	if s.ABI != 0 && len(s.Regs) > 0 {
		fmt.Printf("  regs    : IP=0x%016x SP=0x%016x BP=0x%016x\n",
			perfreader.RegIP(s.Regs), perfreader.RegSP(s.Regs), perfreader.RegBP(s.Regs))
		fmt.Printf("            (%d regs total)\n", len(s.Regs))
	} else {
		fmt.Printf("  regs    : ABI=NONE — user regs not captured (kernel-mode sample?)\n")
	}
	if len(s.Stack) > 0 {
		fmt.Printf("  stack   : addr=0x%016x len=%d bytes\n", s.StackAddr, len(s.Stack))
		// Print the first 4 u64 words of the stack so we can eyeball that
		// the return-address area is populated.
		fmt.Printf("            [% 4x % 4x % 4x % 4x ...]\n",
			le64(s.Stack, 0), le64(s.Stack, 8), le64(s.Stack, 16), le64(s.Stack, 24))
	} else {
		fmt.Printf("  stack   : (empty)\n")
	}
	fmt.Println()
}

func le64(b []byte, off int) uint64 {
	if off+8 > len(b) {
		return 0
	}
	_ = b[off+7]
	return uint64(b[off]) | uint64(b[off+1])<<8 | uint64(b[off+2])<<16 | uint64(b[off+3])<<24 |
		uint64(b[off+4])<<32 | uint64(b[off+5])<<40 | uint64(b[off+6])<<48 | uint64(b[off+7])<<56
}
