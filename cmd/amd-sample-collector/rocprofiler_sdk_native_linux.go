//go:build linux && cgo

package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func runRocprofilerSDKNative(cfg collectorConfig) error {
	if err := probeSharedLibrary(cfg.rocprofilerSDKLibrary); err != nil {
		return fmt.Errorf("load rocprofiler-sdk native library: %w", err)
	}
	return fmt.Errorf("rocprofiler-sdk native collector loaded library but capture is not implemented")
}

func probeSharedLibrary(path string) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	handle := C.dlopen(cpath, C.RTLD_LAZY|C.RTLD_LOCAL)
	if handle == nil {
		if msg := C.dlerror(); msg != nil {
			return fmt.Errorf("%s", C.GoString(msg))
		}
		return fmt.Errorf("unknown dlopen failure")
	}
	defer C.dlclose(handle)
	return nil
}
