//go:build !windows
// +build !windows

package image

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFileExclusiveNonblock takes a non-blocking exclusive advisory lock.
// Returns an error if the lock is already held by another process.
func lockFileExclusiveNonblock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockFile(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
