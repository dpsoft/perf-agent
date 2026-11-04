package otlp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Cooperative ownership within one user's shared filesystem. Different users,
// containers and other agents are not coordinated. Files must never be unlinked
// on release: a replacement inode would permit two writers to own the same GPU.
type deviceLocks struct {
	dir, instance string
	files         map[string]*os.File // nil records contention; no takeover this run
}

func newDeviceLocks(dir, instance string) (*deviceLocks, error) {
	if dir == "" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		dir = filepath.Join(base, fmt.Sprintf("perf-agent-otlp-%d", os.Getuid()))
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Lstat(dir, &stat); err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("OTLP lock directory must be a private directory owned by the current user")
	}
	return &deviceLocks{dir: dir, instance: instance, files: make(map[string]*os.File)}, nil
}

func (l *deviceLocks) acquire(uuid string) (bool, error) {
	if f, ok := l.files[uuid]; ok {
		return f != nil, nil
	}
	if len(l.files) >= maxDevices {
		return false, errors.New("OTLP device limit reached")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(uuid+"\x00"+l.instance)))
	fd, err := unix.Open(filepath.Join(l.dir, key+".lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(fd), key)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			l.files[uuid] = nil
			return false, nil
		}
		return false, err
	}
	l.files[uuid] = f
	return true, nil
}
func (l *deviceLocks) close() {
	for _, f := range l.files {
		if f != nil {
			_ = f.Close()
		}
	}
}
