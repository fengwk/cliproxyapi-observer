//go:build linux || darwin

package engine

import (
	"os"
	"path/filepath"
	"syscall"
)

func openPrivateFile(path string) (*os.File, error) {
	fd, e := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func withInterruptionLock(markerPath string, fn func() error) error {
	// One stable, nonremoved lock per database, shared by all native runtimes.
	f, e := openPrivateFile(filepath.Dir(markerPath) + ".lock")
	if e != nil {
		return e
	}
	defer f.Close()
	for {
		e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if e != syscall.EINTR {
			break
		}
	}
	if e != nil {
		return e
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
