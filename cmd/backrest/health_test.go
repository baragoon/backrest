package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/baragoon/backrest/internal/env"
	"github.com/gofrs/flock"
)

func TestCheckHealth(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		probeHost  string
		statusCode int
		body       string
		redirect   bool
		wantErr    bool
	}{
		{name: "healthy", host: "127.0.0.1", probeHost: "127.0.0.1", statusCode: http.StatusOK, body: healthCheckResponse},
		{name: "wildcard bind address", host: "0.0.0.0", probeHost: "127.0.0.1", statusCode: http.StatusOK, body: healthCheckResponse},
		{name: "unhealthy", host: "127.0.0.1", probeHost: "127.0.0.1", statusCode: http.StatusServiceUnavailable, wantErr: true},
		{name: "unrelated service", host: "127.0.0.1", probeHost: "127.0.0.1", statusCode: http.StatusOK, body: "another service", wantErr: true},
		{name: "redirect", host: "127.0.0.1", probeHost: "127.0.0.1", redirect: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != healthCheckPath {
					http.NotFound(w, r)
					return
				}
				if tt.redirect {
					if r.URL.Query().Get("followed") == "true" {
						backrestHealthHandler(w, r)
						return
					}
					http.Redirect(w, r, healthCheckPath+"?followed=true", http.StatusFound)
					return
				}
				if tt.statusCode == http.StatusOK && tt.body == healthCheckResponse {
					backrestHealthHandler(w, r)
					return
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			address := net.JoinHostPort(tt.host, port)
			probeURL, err := healthProbeURL(address)
			if err != nil {
				t.Fatal(err)
			}
			if want := net.JoinHostPort(tt.probeHost, port); probeURL.Host != want {
				t.Fatalf("healthProbeURL().Host = %q, want %q", probeURL.Host, want)
			}
			if err := checkHealth(address); (err != nil) != tt.wantErr {
				t.Fatalf("checkHealth() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHealthBindAddressFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bind-address")
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", path)

	const address = "127.0.0.1:1234"
	if err := writeHealthBindAddress(address); err != nil {
		t.Fatal(err)
	}
	got, err := healthBindAddress()
	if err != nil {
		t.Fatal(err)
	}
	if got != address {
		t.Fatalf("healthBindAddress() = %q, want %q", got, address)
	}
}

func TestHealthBindAddressFileUsesTMPDIR(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "cache", "tmp")
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", "")
	t.Setenv("BACKREST_HEALTHCHECK", "1")
	t.Setenv("TMPDIR", tempDir)

	const address = "127.0.0.1:1234"
	path, enabled := healthBindAddressFile(uint32(os.Getpid()), address)
	if !enabled {
		t.Fatal("health bind address file is disabled")
	}
	if filepath.Dir(path) != tempDir {
		t.Fatalf("health bind address file directory = %q, want %q", filepath.Dir(path), tempDir)
	}
	if err := writeHealthBindAddress(address); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata healthBindAddressMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Address != address || metadata.PID != uint32(os.Getpid()) || metadata.StartID == "" {
		t.Fatalf("saved health metadata = %+v, want address %q, current PID, and start ID", metadata, address)
	}
}

func TestHealthBindAddressFilesAreDistinct(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", "")
	t.Setenv("BACKREST_HEALTHCHECK", "1")
	t.Setenv("TMPDIR", tempDir)

	const address = "127.0.0.1:1234"
	firstInstancePath, _ := healthBindAddressFile(uint32(os.Getpid()), address)
	secondInstancePath, _ := healthBindAddressFile(uint32(os.Getpid()+1), address)
	if firstInstancePath == secondInstancePath {
		t.Fatal("different instances share a health-address file")
	}

	firstAddress := address
	secondAddress := "127.0.0.1:5678"
	firstPath, _ := healthBindAddressFile(uint32(os.Getpid()), firstAddress)
	secondPath, _ := healthBindAddressFile(uint32(os.Getpid()), secondAddress)
	if firstPath == secondPath {
		t.Fatal("different bind addresses share a health-address file")
	}
	if err := writeHealthBindAddress(firstAddress); err != nil {
		t.Fatal(err)
	}
	if err := writeHealthBindAddress(secondAddress); err != nil {
		t.Fatal(err)
	}
	if _, err := healthBindAddress(); err == nil || !strings.Contains(err.Error(), "multiple Backrest health address files") {
		t.Fatalf("healthBindAddress() error = %v, want ambiguous-file error", err)
	}
}

func TestHealthBindAddressIgnoresExitedProcess(t *testing.T) {
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", "")
	t.Setenv("BACKREST_HEALTHCHECK", "1")
	t.Setenv("TMPDIR", t.TempDir())

	const stalePID uint32 = 1 << 30
	path, enabled := healthBindAddressFile(stalePID, "127.0.0.1:1234")
	if !enabled {
		t.Fatal("health bind address file is disabled")
	}
	data, err := json.Marshal(healthBindAddressMetadata{
		PID:     stalePID,
		StartID: "stale",
		Address: "127.0.0.1:1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := healthBindAddress(); err != nil || got != env.BindAddress() {
		t.Fatalf("healthBindAddress() = %q, %v; want fallback %q", got, err, env.BindAddress())
	}
}

func TestHealthBindAddressRejectsReusedPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bind-address")
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", path)
	data, err := json.Marshal(healthBindAddressMetadata{
		PID:     uint32(os.Getpid()),
		StartID: "previous-process",
		Address: "127.0.0.1:1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := healthBindAddress(); err != nil || got != env.BindAddress() {
		t.Fatalf("healthBindAddress() = %q, %v; want fallback %q", got, err, env.BindAddress())
	}
}

func TestRemoveHealthBindAddressPreservesReplacement(t *testing.T) {
	const address = "127.0.0.1:1234"
	for _, tt := range []struct {
		name string
		pid  uint32
	}{
		{name: "different process", pid: uint32(os.Getpid() + 1)},
		{name: "reused pid", pid: uint32(os.Getpid())},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bind-address")
			t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", path)

			if err := writeHealthBindAddress(address); err != nil {
				t.Fatal(err)
			}
			replacement := healthBindAddressMetadata{
				PID:     tt.pid,
				StartID: "replacement",
				Address: address,
			}
			data, err := json.Marshal(replacement)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}

			removeHealthBindAddress(address)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("replacement health record was removed: %v", err)
			}
			var metadata healthBindAddressMetadata
			if err := json.Unmarshal(got, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata != replacement {
				t.Fatalf("health record = %+v, want replacement %+v", metadata, replacement)
			}
		})
	}
}

func TestHealthPIDRange(t *testing.T) {
	if healthPIDInRange(0) || !healthPIDInRange(1) {
		t.Fatal("health PID range must accept positive PIDs and reject zero")
	}
	if runtime.GOOS == "windows" {
		if !healthPIDInRange(uint64(^uint32(0))) || healthPIDInRange(uint64(^uint32(0))+1) {
			t.Fatal("health PID range does not match the Windows PID type")
		}
		return
	}
	const maxUnixPID = uint64(1<<31 - 1)
	if !healthPIDInRange(maxUnixPID) || healthPIDInRange(maxUnixPID+1) {
		t.Fatal("health PID range does not match the Unix PID type")
	}
}

func TestHealthBindAddressUsesEphemeralListenerAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bind-address")
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", path)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	want := listener.Addr().String()
	if err := writeHealthBindAddress(want); err != nil {
		t.Fatal(err)
	}
	if got, err := healthBindAddress(); err != nil || got != want {
		t.Fatalf("healthBindAddress() = %q, %v; want %q", got, err, want)
	}
}

func TestWriteHealthBindAddressReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	path := filepath.Join(dir, "bind-address")
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", path)

	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating symlink requires Windows privileges: %v", err)
		}
		t.Fatal(err)
	}

	const address = "127.0.0.1:1234"
	if err := writeHealthBindAddress(address); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("metadata path is not a regular file: %v", info.Mode())
	}
	if got, err := healthBindAddress(); err != nil || got != address {
		t.Fatalf("healthBindAddress() = %q, %v; want %q", got, err, address)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "unchanged" {
		t.Fatalf("symlink target contents = %q, %v; want unchanged", got, err)
	}
}

func TestWriteHealthBindAddressCleansUpAfterRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bind-address")
	t.Setenv("BACKREST_HEALTH_BIND_ADDRESS_FILE", path)
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}

	err := writeHealthBindAddress("127.0.0.1:1234")
	var renameErr *os.LinkError
	if !errors.As(err, &renameErr) || !strings.HasPrefix(err.Error(), "writing healthcheck bind address: ") {
		t.Fatalf("writeHealthBindAddress() = %v; want wrapped rename error", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected the original directory and lock file, got %v", entries)
	}
	for _, entry := range entries {
		if entry.Name() == filepath.Base(path) && entry.IsDir() {
			continue
		}
		if entry.Name() == ".backrest-health-bind-address.lock" && entry.Type().IsRegular() {
			continue
		}
		t.Fatalf("unexpected file after failed publication: %s", entry.Name())
	}
}

func TestHealthBindAddressLockTimesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bind-address")
	lock := flock.New(filepath.Join(dir, ".backrest-health-bind-address.lock"), flock.SetPermissions(0666))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	called := false
	err := withHealthBindAddressLock(path, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("withHealthBindAddressLock() error = %v, want deadline exceeded", err)
	}
	if called {
		t.Fatal("health-address operation ran without acquiring the lock")
	}
}
