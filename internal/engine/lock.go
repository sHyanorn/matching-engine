package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// lockDir locks dir for this engine, so no other engine, in this process
// or another, uses it until unlockDir (D028). The lock is held on
// <dir>/LOCK, which stays behind: removing it would let two engines lock
// two different files.
func lockDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, "LOCK")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close() // err is the error to report
		if errors.Is(err, ErrDataDirInUse) {
			return nil, fmt.Errorf("%w: %s", ErrDataDirInUse, dir)
		}
		return nil, fmt.Errorf("engine: locking %s: %w", path, err)
	}
	return f, nil
}

// unlockDir releases a lock taken by lockDir.
func unlockDir(f *os.File) error {
	return errors.Join(unlockFile(f), f.Close())
}
