//go:build !windows

package usagesync

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func lock(directory string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(directory, "sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrSyncRunning
		}
		return nil, fmt.Errorf("lock usage state: %w", err)
	}
	return func() { file.Close() }, nil
}
func replaceFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
