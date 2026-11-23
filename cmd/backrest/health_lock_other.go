//go:build !linux && !darwin && !windows

package main

import (
	"fmt"
	"runtime"
)

type healthLock struct{}

func privateHealthLockDir() (string, error) { return "", unsupportedHealthLock() }

func openHealthLock(string) (healthLock, error) { return healthLock{}, unsupportedHealthLock() }
func (healthLock) tryLock() (bool, error)       { return false, unsupportedHealthLock() }
func (healthLock) unlock() error                { return nil }
func (healthLock) close()                       {}

func unsupportedHealthLock() error {
	return fmt.Errorf("healthcheck file locking is unsupported on %s", runtime.GOOS)
}
