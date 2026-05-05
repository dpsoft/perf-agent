//go:build !linux || !cgo

package main

import "fmt"

func runRocprofilerSDKNative(cfg collectorConfig) error {
	return fmt.Errorf("rocprofiler-sdk native mode requires linux+cgo support")
}
