//go:build darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHealthProcessIdentityRejectsZombie(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestHealthProcessIdentityZombieHelper$")
	command.Env = append(os.Environ(), "BACKREST_HEALTH_ZOMBIE_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Wait()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		process, err := unix.SysctlKinfoProc("kern.proc.pid", command.Process.Pid)
		if errors.Is(err, unix.ESRCH) {
			t.Fatal("child process was reaped before zombie state could be checked")
		}
		if err == nil && process.Proc.P_stat == darwinZombieProcessState {
			_, running, err := healthProcessIdentity(uint32(command.Process.Pid))
			if err != nil {
				t.Fatal(err)
			}
			if running {
				t.Fatal("healthProcessIdentity() reports a zombie process as running")
			}
			if err := command.Wait(); err != nil {
				t.Fatal(err)
			}
			waited = true
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child process did not become a zombie")
}

func TestHealthProcessIdentityZombieHelper(t *testing.T) {
	if os.Getenv("BACKREST_HEALTH_ZOMBIE_HELPER") == "1" {
		os.Exit(0)
	}
}
