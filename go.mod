module perf-agent

go 1.24.0

require (
	github.com/HdrHistogram/hdrhistogram-go v1.2.0
	github.com/cespare/xxhash/v2 v2.3.0
	github.com/cilium/ebpf v0.16.0
	github.com/elastic/go-perf v0.0.0-20241029065020-30bec95324b8
	github.com/google/pprof v0.0.0-20240727154555-813a5fbdbec8
	github.com/iovisor/gobpf v0.2.0
	github.com/klauspost/compress v1.17.10
	github.com/stretchr/testify v1.11.1
	golang.org/x/sys v0.38.0
	kernel.org/pub/linux/libs/security/libcap/cap v1.2.73
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/exp v0.0.0-20230224173230-c95f2b4c22f2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	kernel.org/pub/linux/libs/security/libcap/psx v1.2.73 // indirect
)
