//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || windows)

package engine

import "os"

// Elsewhere the data directory is not locked.

func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
