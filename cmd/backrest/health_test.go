package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckHealth(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		statusCode int
		wantErr    bool
	}{
		{name: "healthy", host: "127.0.0.1", statusCode: http.StatusNoContent},
		{name: "wildcard bind address", host: "0.0.0.0", statusCode: http.StatusNoContent},
		{name: "unhealthy", host: "127.0.0.1", statusCode: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if err := checkHealth(fmt.Sprintf("%s:%s", tt.host, port)); (err != nil) != tt.wantErr {
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
	if err := writeHealthBindAddress(address); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(tempDir, "backrest-health-bind-address"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != address {
		t.Fatalf("saved bind address = %q, want %q", got, address)
	}
}
