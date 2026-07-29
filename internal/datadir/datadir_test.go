package datadir

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrepareCreatesSecureLayoutAndStableKeys(t *testing.T) {
	layout, err := Prepare(filepath.Join(t.TempDir(), "data"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{layout.Spool, layout.Journal, layout.Diagnostics, layout.BrowserTemporary, layout.Run, layout.Keys} {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			t.Fatalf("%s: %v", path, statErr)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != DirectoryMode {
			t.Fatalf("%s mode = %04o", path, info.Mode().Perm())
		}
	}
	keyPath := filepath.Join(layout.Keys, "master.key")
	first, err := LoadOrCreateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || len(first) != KeyBytes {
		t.Fatal("key was not stable")
	}
}

func TestPrepareRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires privileges on Windows")
	}
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, DirectoryMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(link, "", ""); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestRecoverDirectoryTightensOwnedPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission contract")
	}
	path := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RecoverDirectory(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != DirectoryMode {
		t.Fatalf("mode=%04o want=%04o", info.Mode().Perm(), DirectoryMode)
	}
}
