package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func privateHealthLockDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// The directory inherits the ACL of the user's profile.
	dir := filepath.Join(home, ".backrest-health-locks")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("healthcheck lock directory %q is not a directory", dir)
	}
	return dir, nil
}

type healthLock windows.Handle

func openHealthLock(path string) (healthLock, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS,
		windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	kind, err := windows.GetFileType(handle)
	if err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if kind == windows.FILE_TYPE_DISK {
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			windows.CloseHandle(handle)
			return 0, err
		}
	}
	if kind != windows.FILE_TYPE_DISK || info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		windows.CloseHandle(handle)
		return 0, fmt.Errorf("healthcheck lock %q is not a regular file", path)
	}
	return healthLock(handle), nil
}

func (lock healthLock) tryLock() (bool, error) {
	err := windows.LockFileEx(windows.Handle(lock), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func (lock healthLock) unlock() error {
	return windows.UnlockFileEx(windows.Handle(lock), 0, 1, 0, &windows.Overlapped{})
}

func (lock healthLock) close() { _ = windows.CloseHandle(windows.Handle(lock)) }
