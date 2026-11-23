//go:build !linux && !darwin && !windows

package main

import (
	"fmt"
	"runtime"
)

func healthPIDInRange(pid uint64) bool {
	return pid > 0 && pid <= uint64(1<<31-1)
}

func healthProcessIdentity(uint32) (string, bool, error) {
	return "", false, fmt.Errorf("healthcheck process start identifiers are unsupported on %s", runtime.GOOS)
}
