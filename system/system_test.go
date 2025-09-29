package system

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestObtainLock verifies the basic functionality of ObtainLock, ensuring it
// creates a lock file and that the cleanup function removes it.
func TestObtainLock(t *testing.T) {
	lockFile := filepath.Join(t.TempDir(), "googet.lock")
	cleanup, err := ObtainLock(lockFile, 0)
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

// TestObtainLock_StaleLock ensures that ObtainLock can successfully acquire a
// lock even if a stale lock file exists.
func TestObtainLock_StaleLock(t *testing.T) {
	lockFile := filepath.Join(t.TempDir(), "googet.lock")
	// Create an initial lock file. We write the PID of the current process, but
	// close the file handle so there's no actual lock held. This simulates a
	// stale lock from a dead process.
	if err := os.WriteFile(lockFile, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	// Make the lock file appear stale.
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockFile, staleTime, staleTime); err != nil {
		t.Fatalf("os.Chtimes: %v", err)
	}

	// Now, try to obtain the lock again. It should succeed because the existing
	// lock is stale. Note that the PID of the test process is written into the
	// lockfile, but it isn't killed because the name does not match googet.
	cleanup, err := ObtainLock(lockFile, 1*time.Hour)
	if err != nil {
		t.Fatalf("ObtainLock with stale lock: %v", err)
	}
	if cleanup == nil {
		t.Fatalf("ObtainLock got nil cleanup, want non-nil")
	}

	fi, err := os.Stat(lockFile)
	if err != nil {
		t.Fatalf("os.Stat after obtaining lock: %v", err)
	}
	if age := time.Since(fi.ModTime()); age > 1*time.Minute {
		t.Errorf("lock file age is %v, want < 1 minute; stale file was likely not removed and re-created", age)
	}

	cleanup()
	if _, err := os.Stat(lockFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("os.Stat(%v): lockfile still exists after cleanup", lockFile)
	}
}

// TestObtainLock_PID verifies that ObtainLock writes the correct process ID
// into the lock file.
func TestObtainLock_PID(t *testing.T) {
	lockFile := filepath.Join(t.TempDir(), "googet.lock")
	cleanup, err := ObtainLock(lockFile, 0)
	if err != nil {
		t.Fatalf("ObtainLock: %v", err)
	}
	defer cleanup()
	content, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	pidStr := strings.TrimSpace(string(content))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("strconv.Atoi(%q): %v", pidStr, err)
	}
	if got, want := pid, os.Getpid(); got != want {
		t.Errorf("lockfile pid got %d, want %d", got, want)
	}
}

// TestObtainLock_Locked ensures that once a lock is obtained, subsequent calls
// to ObtainLock will block until the lock is released.
func TestObtainLock_Locked(t *testing.T) {
	lockFile := filepath.Join(t.TempDir(), "googet.lock")
	cleanup1, err := ObtainLock(lockFile, 0)
	if err != nil {
		t.Fatalf("ObtainLock: %v", err)
	}

	type obtainLockResult struct {
		cleanup func()
		err     error
	}
	resultCh := make(chan obtainLockResult)
	go func() {
		cleanup2, err := ObtainLock(lockFile, 1*time.Hour)
		resultCh <- obtainLockResult{cleanup2, err}
	}()

	select {
	case res := <-resultCh:
		t.Fatalf("ObtainLock should have blocked, but returned immediately with result: %+v", res)
	case <-time.After(1 * time.Second):
		// It's blocking as expected.
	}

	cleanup1()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("Second ObtainLock failed with: %v", res.err)
		}
		if res.cleanup == nil {
			t.Fatal("Second ObtainLock returned nil cleanup")
		}
		res.cleanup() // Clean up the second lock.
	case <-time.After(1 * time.Second): // Should be quick to get the lock now.
		t.Fatal("Second ObtainLock timed out after releasing first lock")
	}
}
