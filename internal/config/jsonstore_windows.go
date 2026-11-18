//go:build windows
// +build windows

package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	v1 "github.com/baragoon/backrest/gen/go/v1"
	"github.com/hectane/go-acl"
	"golang.org/x/sys/windows"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	configKeepVersions = 10
)

// windowsTempWriter provides a Windows-compatible atomic write helper.
// It creates a temporary file with restrictive DACL, writes data, and atomically replaces the target.
type windowsTempWriter struct {
	tempPath string
	file     *os.File
	ownerSID *windows.SID
	renamed  bool
}

// newWindowsTempWriter creates a temporary file in the same directory as the target
// with restrictive DACL (owner-only access). The ownerSID must be valid; a nil SID
// results in an error before any file is created. Uses exclusive creation with retry
// on collision to avoid overwriting concurrent writers' temporary files.
func newWindowsTempWriter(dir, targetPath string, ownerSID *windows.SID) (*windowsTempWriter, error) {
	if ownerSID == nil {
		return nil, errors.New("owner SID is required")
	}

	const maxRetries = 100
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Generate a random suffix for the temp file to avoid collisions with concurrent writes.
		var randBytes [8]byte
		if _, err := rand.Read(randBytes[:]); err != nil {
			return nil, fmt.Errorf("generate random suffix: %w", err)
		}
		randSuffix := hex.EncodeToString(randBytes[:])
		tempPath := filepath.Join(dir, ".tmp."+randSuffix)

		// Try to create the file exclusively. If it already exists, retry with a new suffix.
		// Use os.OpenFile with os.O_CREATE|os.O_EXCL for exclusive creation.
		file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // Collision; retry with new suffix
			}
			return nil, fmt.Errorf("create temp file: %w", err)
		}

		// Apply restrictive DACL: owner has full access (0x1f01ff), everyone else denied.
		if err := acl.Apply(tempPath, true, false, acl.GrantSid(0x1f01ff, ownerSID)); err != nil {
			// FAT and other volumes without ACLs retain the previous writable
			// file behavior. Never hide access-denied or other ACL failures.
			if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
				err = file.Chmod(0600)
			}
			if err != nil {
				file.Close()
				os.Remove(tempPath)
				return nil, fmt.Errorf("set file permissions: %w", err)
			}
		}

		return &windowsTempWriter{
			tempPath: tempPath,
			file:     file,
			ownerSID: ownerSID,
			renamed:  false,
		}, nil
	}

	return nil, fmt.Errorf("failed to create temp file after %d attempts", maxRetries)
}

// Write writes data to the temporary file.
func (w *windowsTempWriter) Write(p []byte) (int, error) {
	return w.file.Write(p)
}

// CloseAtomicallyReplace closes the temporary file and atomically replaces the target.
func (w *windowsTempWriter) CloseAtomicallyReplace(targetPath string) error {
	if err := w.file.Sync(); err != nil {
		return err
	}
	if err := w.file.Close(); err != nil {
		os.Remove(w.tempPath)
		return err
	}

	from, err := windows.UTF16PtrFromString(w.tempPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return err
	}

	// Mark this writer as having successfully renamed; Cleanup should not remove tempPath now.
	w.renamed = true
	return nil
}

// Cleanup removes the temporary file if it still exists and was not successfully renamed.
// Once CloseAtomicallyReplace succeeds, this is a no-op for the successful path.
func (w *windowsTempWriter) Cleanup() {
	w.file.Close()
	if !w.renamed {
		os.Remove(w.tempPath)
	}
}

// getOwnerSID returns the SID of the current process owner.
func getOwnerSID() (*windows.SID, error) {
	// Get the current process token and its user information
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()

	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}

	return user.User.Sid, nil
}

type JsonFileStore struct {
	Path string
	mu   sync.Mutex
}

var _ ConfigStore = &JsonFileStore{}

func (f *JsonFileStore) Get() (*v1.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.get()
}

func (f *JsonFileStore) Update(config *v1.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.update(config)
}

func (f *JsonFileStore) Transform(fn func(cfg *v1.Config) (*v1.Config, error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	current, err := f.get()
	if err != nil {
		return err
	}

	cloned := proto.Clone(current).(*v1.Config)
	result, err := fn(cloned)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return f.update(result)
}

// get reads the config from disk. Must be called with mu held.
func (f *JsonFileStore) get() (*v1.Config, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config v1.Config
	if err = (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &config, nil
}

// update writes the config to disk. Must be called with mu held.
// On Windows, uses renameio with DACL permissions for owner-only access.
func (f *JsonFileStore) update(config *v1.Config) error {
	data, err := protojson.MarshalOptions{
		Indent:    "  ",
		Multiline: true,
	}.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	err = os.MkdirAll(filepath.Dir(f.Path), 0755)
	if err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	// backup the old config file
	if err := f.makeBackup(); err != nil {
		return fmt.Errorf("backup config file: %w", err)
	}

	// Get the current process owner's SID for DACL
	ownerSID, err := getOwnerSID()
	if err != nil {
		return fmt.Errorf("get owner SID: %w", err)
	}

	// Use Windows-specific temporary file writer with restrictive DACL
	w, err := newWindowsTempWriter(filepath.Dir(f.Path), f.Path, ownerSID)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer w.Cleanup()

	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}

	if err := w.CloseAtomicallyReplace(f.Path); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	return nil
}

func (f *JsonFileStore) makeBackup() error {
	curConfig, err := os.ReadFile(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	// Get the current process owner's SID for DACL
	ownerSID, err := getOwnerSID()
	if err != nil {
		return fmt.Errorf("get owner SID: %w", err)
	}

	// backup the current config file with restrictive DACL
	backupName := fmt.Sprintf("%s.bak.%s", f.Path, time.Now().Format("2006-01-02-15-04-05"))
	w, err := newWindowsTempWriter(filepath.Dir(backupName), backupName, ownerSID)
	if err != nil {
		return err
	}
	defer w.Cleanup()

	if _, err := w.Write(curConfig); err != nil {
		return err
	}

	if err := w.CloseAtomicallyReplace(backupName); err != nil {
		return err
	}

	// only keep the last 10 versions
	files, err := filepath.Glob(f.Path + ".bak.*")
	if err != nil {
		return err
	}
	if len(files) > configKeepVersions {
		for _, file := range files[:len(files)-configKeepVersions] {
			if err := os.Remove(file); err != nil {
				return err
			}
		}
	}

	return nil
}
