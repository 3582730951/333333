// Package datadir owns the relay's persistent filesystem layout and key files.
// It deliberately rejects ambiguous paths (symlinks, foreign owners and permissive
// key modes) before any database or background worker is started.
package datadir

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	DirectoryMode = 0o700
	SecretMode    = 0o600
	KeyBytes      = 32
)

type Layout struct {
	Root             string
	Spool            string
	Journal          string
	Diagnostics      string
	BrowserTemporary string
	Run              string
	Keys             string
}

// Prepare creates and validates the complete persistent layout. An explicit
// spool or journal directory is supported for compatibility, but receives the
// exact same security checks as a directory below root.
func Prepare(root, explicitSpool, explicitJournal string) (Layout, error) {
	if root == "" {
		root = "data"
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve data directory: %w", err)
	}
	layout := Layout{
		Root:             absolute,
		Spool:            filepath.Join(absolute, "spool"),
		Journal:          filepath.Join(absolute, "journal"),
		Diagnostics:      filepath.Join(absolute, "diagnostics"),
		BrowserTemporary: filepath.Join(absolute, "tmp", "browser"),
		Run:              filepath.Join(absolute, "run"),
		Keys:             filepath.Join(absolute, "keys"),
	}
	if explicitSpool != "" {
		layout.Spool, err = filepath.Abs(explicitSpool)
		if err != nil {
			return Layout{}, fmt.Errorf("resolve spool directory: %w", err)
		}
	}
	if explicitJournal != "" {
		layout.Journal, err = filepath.Abs(explicitJournal)
		if err != nil {
			return Layout{}, fmt.Errorf("resolve journal directory: %w", err)
		}
	}

	paths := []string{
		layout.Root,
		layout.Spool,
		layout.Journal,
		layout.Diagnostics,
		filepath.Dir(layout.BrowserTemporary),
		layout.BrowserTemporary,
		layout.Run,
		layout.Keys,
	}
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		if err := EnsureDirectory(clean); err != nil {
			return Layout{}, err
		}
	}
	return layout, nil
}

// EnsureDirectory is also safe to call while serving. It recreates a removed
// leaf directory, while refusing to follow a replacement symlink.
func EnsureDirectory(path string) error {
	if path == "" {
		return errors.New("data directory path is empty")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, DirectoryMode); err != nil {
		return fmt.Errorf("create data directory %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect data directory %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("data directory %s is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("data directory %s is not a directory", path)
	}
	if err := verifyOwner(info); err != nil {
		return fmt.Errorf("data directory %s: %w", path, err)
	}
	if err := os.Chmod(path, DirectoryMode); err != nil {
		return fmt.Errorf("secure data directory %s: %w", path, err)
	}
	if err := writableProbe(path); err != nil {
		return fmt.Errorf("data directory %s is not writable: %w", path, err)
	}
	return nil
}

// RecoverDirectory is the inexpensive serving-path check. Existing directories
// are validated without a probe write; a missing directory receives the full
// creation and writability preflight.
func RecoverDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return EnsureDirectory(path)
	}
	if err != nil {
		return fmt.Errorf("inspect data directory %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("data directory %s is not a regular directory", path)
	}
	if err := verifyOwner(info); err != nil {
		return fmt.Errorf("data directory %s: %w", path, err)
	}
	if info.Mode().Perm() != DirectoryMode {
		// Permission drift on an owned, non-symlink directory is recoverable. Tighten
		// it in place; foreign ownership remains a hard failure above.
		if err := os.Chmod(path, DirectoryMode); err != nil {
			return fmt.Errorf("secure data directory %s: %w", path, err)
		}
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	volume := filepath.VolumeName(absolute)
	if volume != "" {
		current = volume + string(filepath.Separator)
	}
	relative := absolute[len(current):]
	for _, component := range splitPath(relative) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("inspect path component %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %s is a symlink", current)
		}
	}
	return nil
}

func splitPath(path string) []string {
	var out []string
	for path != "" && path != "." {
		dir, base := filepath.Split(path)
		if base != "" {
			out = append([]string{base}, out...)
		}
		path = filepath.Clean(dir)
		if path == string(filepath.Separator) || path == "." {
			break
		}
	}
	return out
}

func writableProbe(dir string) error {
	file, err := os.CreateTemp(dir, ".preflight-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(SecretMode); err == nil {
		_, err = file.Write([]byte{0})
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// LoadOrCreateKey reads exactly 32 random bytes from a secure, persistent key
// file. Creation uses O_EXCL and fsync so concurrent starters cannot overwrite
// one another or observe a partially-written key.
func LoadOrCreateKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("key path is empty")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	info, statErr := os.Lstat(path)
	if statErr == nil {
		return loadExistingKey(path, info)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect key %s: %w", path, statErr)
	}
	if err := EnsureDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, SecretMode)
	if err == nil {
		key := make([]byte, KeyBytes)
		if _, err = io.ReadFull(rand.Reader, key); err == nil {
			_, err = file.Write(key)
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
			return nil, fmt.Errorf("create key %s: %w", path, err)
		}
		if err = syncDirectory(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("sync key directory: %w", err)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create key %s: %w", path, err)
	}

	info, err = os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect key %s: %w", path, err)
	}
	return loadExistingKey(path, info)
}

// LoadCredentialKey reads a pre-provisioned key from a read-only secret mount
// (for example systemd LoadCredential or a Docker secret). Unlike
// LoadOrCreateKey, it never creates or rewrites the source and permits a
// root-owned file when the service itself runs unprivileged.
func LoadCredentialKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("credential key path is empty")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect credential key %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("credential key %s is not a regular non-symlink file", path)
	}
	if err := verifyCredentialOwner(info); err != nil {
		return nil, fmt.Errorf("credential key %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("credential key %s has unsafe permissions %04o", path, info.Mode().Perm())
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential key %s: %w", path, err)
	}
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("credential key %s must contain exactly %d bytes", path, KeyBytes)
	}
	return key, nil
}

func loadExistingKey(path string, info os.FileInfo) ([]byte, error) {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("key %s is not a regular non-symlink file", path)
	}
	if err := verifyOwner(info); err != nil {
		return nil, fmt.Errorf("key %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key %s has unsafe permissions %04o", path, info.Mode().Perm())
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("key %s must contain exactly %d bytes", path, KeyBytes)
	}
	return key, nil
}

func KeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
