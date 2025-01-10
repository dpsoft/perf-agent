SHELL := /bin/bash
all: build
.PHONY: all

LIBBLAZESYM_SRC := $(abspath /home/diego/github/blazesym/)
LIBBLAZESYM_INC := $(abspath $(LIBBLAZESYM_SRC)/capi/include)
LIBBLAZESYM_OBJ := $(abspath $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a)
ALL_LDFLAGS := $(LDFLAGS) $(EXTRA_LDFLAGS)

#snoop/out/dnswatch_bpf_probe_core.o: bpf/vmlinux.h
#	clang -g -O2 -Wall -fpie -fno-stack-protector -I /usr/include/bpf -target bpf -D__TARGET_ARCH_x86 -I bpf  -c bpf/dnswatch.bpf.c -o $@
#
build: $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a
	CGO_LDFLAGS=" -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release) -lblazesym_c -static " CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release)" go build .
