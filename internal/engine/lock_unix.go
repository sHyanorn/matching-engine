//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package engine

import (
	"errors"
	"os"
	"syscall"
)

// flock, unlike fcntl locks, conflicts between two opens of the file in
// one process too.

func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrDataDirInUse
	}
	return err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
