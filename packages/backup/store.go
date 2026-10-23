package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Object is one stored backup.
type Object struct {
	Key      string // the backup id, e.g. app/20261022T030000Z.dump.age
	Size     int64
	Modified time.Time
}

// Store is where encrypted backups live: an S3-compatible bucket in
// production (S3), a directory locally (Dir).
type Store interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, sha256hex string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]Object, error)
}

// Dir is a Store in a local directory (make db-backup): keys are paths
// under it.
type Dir string

var _ Store = Dir("")

func (d Dir) path(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || filepath.IsAbs(key) {
		return "", fmt.Errorf("backup: invalid key %q", key)
	}
	return filepath.Join(string(d), filepath.FromSlash(key)), nil
}

const backupDirPerm = 0o700 // backup directories: owner only

// Put writes body to key atomically (temp file, then rename).
func (d Dir) Put(_ context.Context, key string, body io.Reader, _ int64, _ string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), backupDirPerm); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("backup: write %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup: write %s: %w", key, err)
	}
	return os.Rename(tmp.Name(), p)
}

// Get opens key.
func (d Dir) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("backup: %s: %w", key, ErrNotFound)
	}
	return f, err
}

// Delete removes key.
func (d Dir) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// List returns every object whose key starts with prefix.
func (d Dir) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(string(d), func(p string, e fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == string(d) {
			return fs.SkipAll // no backups yet
		}
		if err != nil || e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			return err
		}
		rel, err := filepath.Rel(string(d), p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		out = append(out, Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("backup: list: %w", err)
	}
	return out, nil
}
