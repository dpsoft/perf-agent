package main

import (
	"fmt"
	"os"
	"runtime"

	blazesym "github.com/libbpf/blazesym/go"
)

func main() {
	pid := uint32(os.Getpid())
	fmt.Printf("Self PID: %d\n", pid)

	sym, err := blazesym.NewSymbolizer(blazesym.SymbolizerWithCodeInfo(true))
	if err != nil {
		fmt.Printf("NewSymbolizer error: %v\n", err)
		return
	}
	defer sym.Close()

	pc, _, _, _ := runtime.Caller(0)
	fmt.Printf("Testing self address: 0x%x\n", pc)

	result, err := sym.SymbolizeProcessAbsAddrs(
		[]uint64{uint64(pc)},
		pid,
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	)
	fmt.Printf("Self result: %+v\n", result)
	fmt.Printf("Self error: %v\n", err)

	if len(os.Args) > 1 {
		var targetPid uint32
		fmt.Sscanf(os.Args[1], "%d", &targetPid)
		fmt.Printf("\nTarget PID: %d\n", targetPid)

		maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", targetPid))
		if err != nil {
			fmt.Printf("ReadFile maps: %v\n", err)
			return
		}

		var addr uint64
		for _, b := range splitLines(maps) {
			line := string(b)
			if len(line) > 0 {
				fmt.Sscanf(line, "%x-", &addr)
				if addr > 0 {
					break
				}
			}
		}
		fmt.Printf("Testing target address: 0x%x\n", addr)

		result, err = sym.SymbolizeProcessAbsAddrs(
			[]uint64{addr},
			targetPid,
			blazesym.ProcessSourceWithPerfMap(true),
			blazesym.ProcessSourceWithDebugSyms(true),
		)
		fmt.Printf("Target result: %+v\n", result)
		fmt.Printf("Target error: %v\n", err)
	}
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	return lines
}
