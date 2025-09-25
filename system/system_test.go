package system

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestObtainLock(t *testing.T) {
	tempDir := t.TempDir()
	lockFile := filepath.Join(tempDir, "googet.lock")
	cleanup, err := ObtainLock(lockFile)
	if err != nil {
		t.Fatalf("ObtainLock: %v", err)
	}
	if cleanup == nil {
		t.Fatalf("ObtainLock got nil cleanup, want non-nil")
	}
	if _, err := os.Stat(lockFile); errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("os.Stat(%v): lockfile does not exist", lockFile)
	}
	cleanup()
	if _, err := os.Stat(lockFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("os.Stat(%v): lockfile still exists after cleanup", lockFile)
	}
}
