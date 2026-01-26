module github.com/dpsoft/perf-agent/test

go 1.24.0

require (
	github.com/google/pprof v0.0.0-20241210010833-40e02aabc2ad
	github.com/stretchr/testify v1.11.1
	github.com/dpsoft/perf-agent v0.0.0
)

require (
	github.com/HdrHistogram/hdrhistogram-go v1.2.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cilium/ebpf v0.16.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/iovisor/gobpf v0.2.0 // indirect
	github.com/klauspost/compress v1.17.10 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/exp v0.0.0-20230224173230-c95f2b4c22f2 // indirect
	golang.org/x/sys v0.38.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	kernel.org/pub/linux/libs/security/libcap/cap v1.2.73 // indirect
	kernel.org/pub/linux/libs/security/libcap/psx v1.2.73 // indirect
)

replace github.com/dpsoft/perf-agent => ../
