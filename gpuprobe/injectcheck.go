package gpuprobe

// ShimIsMappedIn reports whether pid has the shim at shimPath mapped.
//
// WHY THIS IS NEEDED AT ALL
// -------------------------
// CUDA_INJECTION64_PATH fails OPEN and SILENT. If the driver cannot load the
// library -- wrong path, wrong architecture, a glibc the target does not have
// (issue #121) -- it carries on exactly as if the variable had never been set.
// Nothing is logged by the driver, the workload runs normally, and the profile
// comes out empty. "The shim is broken" and "this workload launched no kernels"
// produce identical output.
//
// The mapping is the one observable that separates them, and it is the same
// fact enrolment already keys on: the shim appears in the victim's maps as
// file-backed segments carrying the inode of the file on disk.
//
// Answers only for THIS pid, not its descendants. A workload launched through
// a wrapper script does its CUDA work in a child, and the shim will be mapped
// there rather than here -- so a false answer is possible and callers must say
// so rather than assert injection failed.
func ShimIsMappedIn(pid int, shimPath string) (bool, error) {
	dev, ino, err := enrollShimIdentity(shimPath)
	if err != nil {
		return false, err
	}
	return procMapsHaveInode("/proc", uint32(pid), dev, ino) //nolint:gosec // a pid from exec.Cmd
}
