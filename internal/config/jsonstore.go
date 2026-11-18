package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	v1 "github.com/baragoon/backrest/gen/go/v1"
	"github.com/google/renameio/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	configKeepVersions = 10
)

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

	t, err := renameio.TempFile(filepath.Dir(f.Path), f.Path)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer t.Cleanup()

	if _, err := t.Write(data); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}

	if err := t.CloseAtomicallyReplace(); err != nil {
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

	// backup the current config file
	backupName := fmt.Sprintf("%s.bak.%s", f.Path, time.Now().Format("2006-01-02-15-04-05"))
	t, err := renameio.TempFile(filepath.Dir(backupName), backupName)
	if err != nil {
		return err
	}
	defer t.Cleanup()

	if _, err := t.Write(curConfig); err != nil {
		return err
	}

	if err := t.CloseAtomicallyReplace(); err != nil {
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
