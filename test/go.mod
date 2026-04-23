module github.com/dpsoft/perf-agent/test

go 1.26.0

require (
	github.com/cilium/ebpf v0.21.0
	github.com/dpsoft/perf-agent v0.0.0
	github.com/google/pprof v0.0.0-20260402051712-545e8a4df936
	github.com/stretchr/testify v1.11.1
	golang.org/x/sys v0.42.0
)

require (
	github.com/HdrHistogram/hdrhistogram-go v1.2.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/iovisor/gobpf v0.2.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/libbpf/blazesym/go v0.0.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	kernel.org/pub/linux/libs/security/libcap/cap v1.2.77 // indirect
	kernel.org/pub/linux/libs/security/libcap/psx v1.2.77 // indirect
)

replace github.com/dpsoft/perf-agent => ../

replace github.com/libbpf/blazesym/go => /home/diego/github/blazesym/go
