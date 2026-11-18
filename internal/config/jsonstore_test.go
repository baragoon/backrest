//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/baragoon/backrest/gen/go/v1"
)

func TestJsonStoreTempCreationFailurePreservesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	// The target name is valid, but renameio's added random suffix exceeds NAME_MAX.
	base := strings.Repeat("x", 250)
	other := filepath.Join(dir, "."+base)
	if err := os.WriteFile(other, []byte("another writer's file"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &JsonFileStore{Path: filepath.Join(dir, base)}
	if err := store.Update(&v1.Config{}); err == nil {
		t.Fatal("expected temporary file creation error")
	}
	if data, err := os.ReadFile(other); err != nil || string(data) != "another writer's file" {
		t.Fatalf("unrelated file changed: %q, %v", data, err)
	}
}
