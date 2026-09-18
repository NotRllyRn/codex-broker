package platform

import (
	"os"
	"syscall"
)

const ServiceID = 10001

func PrepareVolumes(paths ...string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		if err := os.Chown(path, ServiceID, ServiceID); err != nil {
			return err
		}
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return err
	}
	if err := syscall.Setgid(ServiceID); err != nil {
		return err
	}
	return syscall.Setuid(ServiceID)
}
