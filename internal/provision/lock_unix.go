//go:build unix

package provision

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func checkConfigFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0o022 != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("config must be a regular file owned by the current user and not group/world-writable")
	}
	return nil
}

func lock(root string) (func(), error) {
	f, err := os.OpenFile(root+"/.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open provisioning lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another provisioning operation is in progress: %w", err)
		}
		return nil, fmt.Errorf("lock provisioning root: %w", err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
