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
	"unsafe"

	v1 "github.com/baragoon/backrest/gen/go/v1"
	"golang.org/x/sys/windows"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	configKeepVersions = 10
)

// buildRestrictiveSecurityDescriptor creates a security descriptor that allows
// only the owner full access and denies all other access. This is used for
// temporary files to prevent information disclosure.
func buildRestrictiveSecurityDescriptor(ownerSID *windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	// Create an ACL with an explicit grant for the owner using SetEntriesInAcl.
	entries := []windows.ExplicitAccess{
		{
			AccessPermissions: 0x1f01ff, // Full control (FILE_ALL_ACCESS)
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.Trustee{
				MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
				TrusteeForm:              windows.TRUSTEE_IS_SID,
				TrusteeType:              windows.TRUSTEE_IS_USER,
				TrusteeValue:             windows.TrusteeValueFromSID(ownerSID),
			},
		},
	}

	acl, err := windows.SetEntriesInAcl(entries)
	if err != nil {
		return nil, fmt.Errorf("create ACL: %w", err)
	}

	// Create a security descriptor and assign the DACL.
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("create security descriptor: %w", err)
	}

	// Set DACL with protection to prevent inheritance of parent directory permissions.
	if err := sd.SetDACL(acl, true, false); err != nil {
		return nil, fmt.Errorf("set DACL: %w", err)
	}

	// Mark the DACL as protected so inherited permissions don't apply.
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, fmt.Errorf("protect DACL: %w", err)
	}

	return sd, nil
}

// createFileWithSD creates a file with a specified security descriptor.
// It uses windows.CreateFile with exclusive creation and fails if the file exists.
func createFileWithSD(path string, sd *windows.SECURITY_DESCRIPTOR) (*os.File, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("path encoding: %w", err)
	}

	// Create security attributes structure with the security descriptor.
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}

	// Use CreateFile with CREATE_NEW to ensure exclusive creation.
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // No sharing
		sa,
		windows.CREATE_NEW, // Exclusive creation; fail if exists
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}

	// Convert Windows handle to *os.File.
	file := os.NewFile(uintptr(handle), path)
	return file, nil
}

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
// The restrictive security descriptor is applied from file creation to prevent
// other users from opening a read handle during the creation window.
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

		// Create a security descriptor with DACL allowing owner full access.
		// This must be applied at file creation time, not afterward.
		sd, err := buildRestrictiveSecurityDescriptor(ownerSID)
		if err != nil {
			return nil, fmt.Errorf("build security descriptor: %w", err)
		}

		// Try to create the file exclusively with the restrictive security descriptor.
		// Use windows.CreateFile for direct control over security attributes.
		file, err := createFileWithSD(tempPath, sd)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_EXISTS) {
				continue // Collision; retry with new suffix
			}
			return nil, fmt.Errorf("create temp file: %w", err)
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
