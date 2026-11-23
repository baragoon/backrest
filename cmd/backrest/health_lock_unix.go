//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/baragoon/backrest/internal/env"
	"golang.org/x/sys/unix"
)

func privateHealthLockDir() (string, error) {
	parent := env.DataDir()
	info, err := os.Stat(parent)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 ||
		(info.Sys().(*syscall.Stat_t).Uid != 0 && info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid())) {
		return "", fmt.Errorf("healthcheck lock parent %q must not be writable by other users", parent)
	}
	dir := filepath.Join(parent, ".backrest-health-locks")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err = os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("healthcheck lock directory %q must be private and owned by the current user", dir)
	}
	return dir, nil
}

type healthLock int

func openHealthLock(path string) (healthLock, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return -1, err
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return -1, fmt.Errorf("healthcheck lock %q is not a regular file", path)
	}
	return healthLock(fd), nil
}

func (lock healthLock) tryLock() (bool, error) {
	err := unix.Flock(int(lock), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return err == nil, err
}

func (lock healthLock) unlock() error { return unix.Flock(int(lock), unix.LOCK_UN) }
func (lock healthLock) close()        { _ = unix.Close(int(lock)) }
