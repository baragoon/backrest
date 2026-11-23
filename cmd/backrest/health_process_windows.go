//go:build windows

package main

import (
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/windows"
)

func healthPIDInRange(pid uint64) bool {
	return pid > 0 && pid <= uint64(^uint32(0))
}

func healthProcessIdentity(pid uint32) (string, bool, error) {
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		pid,
	)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer windows.CloseHandle(handle)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", false, err
	}
	wait, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return "", false, err
	}
	switch wait {
	case windows.WAIT_OBJECT_0:
		return "", false, nil
	case uint32(windows.WAIT_TIMEOUT):
		startID := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
		return strconv.FormatUint(startID, 10), true, nil
	default:
		return "", false, fmt.Errorf("unexpected process wait result %d", wait)
	}
}
