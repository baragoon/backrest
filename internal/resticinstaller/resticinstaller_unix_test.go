//go:build !windows

package resticinstaller

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baragoon/backrest/internal/env"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFindHelperFailedManagedUpgrade(t *testing.T) {
	oldVersion, oldSemver := RequiredResticVersion, requiredVersionSemver
	RequiredResticVersion, requiredVersionSemver = "0.20.0", mustParseSemVer("0.20.0")
	t.Cleanup(func() {
		RequiredResticVersion, requiredVersionSemver = oldVersion, oldSemver
	})

	archive, err := base64.StdEncoding.DecodeString("QlpoOTFBWSZTWQ7ktCwAAAmRgEAANiWcoCAAMQAACgP1Go9JryW4MhBL6SnTr4u5IpwoSAdyWhYA")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		content     string
		mode        os.FileMode
		failure     string
		wantSuccess bool
	}{
		{"older binary, download fails", "#!/bin/sh\necho 'restic 0.19.1'\n", 0755, "download", true},
		{"older binary, checksums unavailable", "#!/bin/sh\necho 'restic 0.19.1'\n", 0755, "hash", true},
		{"older binary, signature unavailable", "#!/bin/sh\necho 'restic 0.19.1'\n", 0755, "signature", true},
		{"older binary, invalid signature", "#!/bin/sh\necho 'restic 0.19.1'\n", 0755, "invalid signature", true},
		{"newer binary, download fails", "#!/bin/sh\necho 'restic 0.21.0'\n", 0755, "download", true},
		{"missing binary", "", 0, "download", false},
		{"missing binary, verification fails", "", 0, "hash", false},
		{"unexecutable binary", "#!/bin/sh\necho 'restic 0.19.1'\n", 0644, "download", false},
		{"invalid version output", "#!/bin/sh\necho invalid\n", 0755, "download", false},
		{"failing binary", "#!/bin/sh\nexit 1\n", 0755, "download", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			installPath := filepath.Join(dir, "restic")
			if tt.content != "" {
				if err := os.WriteFile(installPath, []byte(tt.content), tt.mode); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(env.EnvVarDataDir, dir)
			t.Setenv(env.EnvVarBinPath, "")
			t.Setenv("PATH", "")

			previousClient := http.DefaultClient
			var requests []string
			http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests = append(requests, req.URL.String())
				body := []byte("invalid signature or checksum")
				switch req.URL.String() {
				case resticDownloadURL(RequiredResticVersion):
					if tt.failure == "download" {
						return nil, errors.New("download unavailable")
					}
					body = archive
				case hashDownloadURL(RequiredResticVersion):
					if tt.failure == "hash" {
						return nil, errors.New("checksums unavailable")
					}
				case sigDownloadURL(RequiredResticVersion):
					if tt.failure == "signature" {
						return nil, errors.New("signature unavailable")
					}
				default:
					t.Fatalf("unexpected URL: %s", req.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previousClient })

			core, logs := observer.New(zap.WarnLevel)
			restoreLogger := zap.ReplaceGlobals(zap.New(core))
			t.Cleanup(restoreLogger)

			got, err := findHelper()
			wantLastURL := resticDownloadURL(RequiredResticVersion)
			if tt.failure == "hash" {
				wantLastURL = hashDownloadURL(RequiredResticVersion)
			} else if tt.failure == "signature" || tt.failure == "invalid signature" {
				wantLastURL = sigDownloadURL(RequiredResticVersion)
			}
			if len(requests) == 0 || requests[len(requests)-1] != wantLastURL {
				t.Errorf("requests = %v, want last request %s", requests, wantLastURL)
			}
			if tt.wantSuccess {
				if err != nil || got != installPath {
					t.Fatalf("findHelper() = %q, %v; want %q, nil", got, err, installPath)
				}
				if entries := logs.FilterMessageSnippet("using existing restic").All(); len(entries) != 1 || !strings.Contains(entries[0].Message, "failed") {
					t.Errorf("expected warning with upgrade failure, got %v", logs.All())
				}
			} else if err == nil || !strings.Contains(err.Error(), "install restic") {
				t.Fatalf("findHelper() = %q, %v; want install error", got, err)
			}
			if tt.content != "" {
				gotContent, err := os.ReadFile(installPath)
				if err != nil || string(gotContent) != tt.content {
					t.Errorf("existing binary changed: content %q, error %v", gotContent, err)
				}
			}
		})
	}
}

func TestFindOrDownloadResticAlreadyInstalled(t *testing.T) {
	installPath := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(installPath, []byte("#!/bin/sh\necho 'restic "+RequiredResticVersion+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	previousClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected download: %s", req.URL)
		return nil, errors.New("unexpected download")
	})}
	t.Cleanup(func() { http.DefaultClient = previousClient })

	if err := findOrDownloadRestic(installPath); err != nil {
		t.Fatal(err)
	}
}

func TestFindOrDownloadResticSuccessfulUpgrade(t *testing.T) {
	archive, err := base64.StdEncoding.DecodeString("QlpoOTFBWSZTWQ7ktCwAAAmRgEAANiWcoCAAMQAACgP1Go9JryW4MhBL6SnTr4u5IpwoSAdyWhYA")
	if err != nil {
		t.Fatal(err)
	}
	entity, err := openpgp.NewEntity("test", "", "test@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var publicKey bytes.Buffer
	writer, err := armor.Encode(&publicKey, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := entity.Serialize(writer); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	oldKey := key
	key = publicKey.Bytes()
	t.Cleanup(func() { key = oldKey })

	sum := sha256.Sum256(archive)
	checksums := []byte(fmt.Sprintf("%x  restic.bz2\n", sum))
	var signature bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&signature, entity, bytes.NewReader(checksums), nil); err != nil {
		t.Fatal(err)
	}
	previousClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		switch req.URL.String() {
		case resticDownloadURL(RequiredResticVersion):
			body = archive
		case hashDownloadURL(RequiredResticVersion):
			body = checksums
		case sigDownloadURL(RequiredResticVersion):
			body = signature.Bytes()
		default:
			t.Fatalf("unexpected URL: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = previousClient })

	installPath := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(installPath, []byte("#!/bin/sh\necho 'restic 0.1.0'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := findOrDownloadRestic(installPath); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(installPath)
	if err != nil || string(content) != "test downloaded binary" {
		t.Fatalf("installed content = %q, error %v", content, err)
	}
	info, err := os.Stat(installPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("installed permissions = %v, want 0755", info.Mode().Perm())
	}
}
