package engine

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// The lock covers every byte of the file, as in Go's cmd/go.
const lockLen = ^uint32(0)

func lockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, lockLen, lockLen, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrDataDirInUse
	}
	return err
}

// unlockFile unlocks at once: Windows may take a while to release the lock
// of a closed file.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockLen, lockLen, new(windows.Overlapped))
}
