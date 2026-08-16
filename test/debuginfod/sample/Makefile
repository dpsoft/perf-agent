CC ?= gcc
# -no-pie: static link-time addresses (so `nm` output == runtime addresses).
# -Wl,--build-id: ensure GNU build-id note (required for debuginfod).
CFLAGS ?= -g -O0 -no-pie -fno-pie -Wl,--build-id

# Outputs:
#   hello.full   = compiled binary with full DWARF + symtab (used for `nm`)
#   hello.debug  = standalone DWARF + symtab (uploaded to debuginfod)
#   hello        = stripped binary with .gnu_debuglink (what we actually run)
#
# This mimics a real production deployment: the running binary is stripped,
# the debug info lives in a symbol server, and the symbolizer must use
# the dispatcher hook to fetch debuginfo on demand.

all: hello hello.full hello.debug

hello.full: hello.c
	$(CC) $(CFLAGS) -o hello.full hello.c

hello.debug: hello.full
	objcopy --only-keep-debug hello.full hello.debug

hello: hello.full hello.debug
	objcopy --strip-debug --strip-unneeded --add-gnu-debuglink=hello.debug hello.full hello
	chmod +x hello

buildid: hello.full
	@readelf -n hello.full | awk '/Build ID/ { print "Build ID:", $$3 }'

clean:
	rm -f hello hello.full hello.debug .build-id

.PHONY: all buildid clean
