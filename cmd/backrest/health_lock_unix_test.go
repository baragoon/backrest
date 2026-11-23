//go:build linux || darwin

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHealthBindAddressLockRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "health.lock")
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_LOCK_FILE", lockPath)
	if err := unix.Mkfifo(lockPath, 0600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- withHealthBindAddressLock(filepath.Join(dir, "bind-address"), func() error {
			t.Error("callback ran with a FIFO lock")
			return nil
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
			t.Fatalf("withHealthBindAddressLock() = %v; want non-regular file error", err)
		}
	case <-time.After(healthBindAddressLockTimeout):
		t.Fatal("opening a FIFO blocked healthcheck lock acquisition")
	}
}
