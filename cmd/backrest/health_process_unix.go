//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func healthPIDInRange(pid uint64) bool {
	return pid > 0 && pid <= uint64(1<<31-1)
}

func healthProcessIdentity(pid uint32) (string, bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	stat := string(data)
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 {
		return "", false, fmt.Errorf("invalid process stat for PID %d", pid)
	}
	fields := strings.Fields(stat[closeParen+1:])
	if len(fields) <= 19 {
		return "", false, fmt.Errorf("incomplete process stat for PID %d", pid)
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", false, nil
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", false, fmt.Errorf("invalid start time for PID %d: %w", pid, err)
	}
	return fields[19], true, nil
}
