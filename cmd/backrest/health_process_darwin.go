//go:build darwin

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const darwinZombieProcessState int8 = 5 // SZOMB from sys/proc.h.

func healthPIDInRange(pid uint64) bool {
	return pid > 0 && pid <= uint64(1<<31-1)
}

func healthProcessIdentity(pid uint32) (string, bool, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EIO) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if process.Proc.P_stat == darwinZombieProcessState {
		return "", false, nil
	}
	startTime := process.Proc.P_starttime
	return fmt.Sprintf("%d:%d", startTime.Sec, startTime.Usec), true, nil
}
