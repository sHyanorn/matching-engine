//go:build !windows

package engine

import (
	"errors"
	"os"
)

// syncDir fsyncs a directory, so the entries created in it survive a
// machine crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
