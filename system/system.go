/*
Copyright 2018 Google Inc. All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package system handles system specific functions.
package system

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

// Verify runs a verify command given a package extraction directory and a PkgSpec struct.
func Verify(dir string, ps *goolib.PkgSpec) error {
	v := ps.Verify
	if v.Path == "" {
		return nil
	}

	logger.Infof("Running verify command: %q", v.Path)
	out, err := oswrap.Create(filepath.Join(dir, "googet_verify.log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	return goolib.Exec(filepath.Join(dir, v.Path), v.Args, v.ExitCodes, out)
}

// isLockFileStale checks if the lock file is older than maxAge.
// It returns false if the file does not exist.
func isLockFileStale(lockFile string, maxAge time.Duration) (bool, error) {
	fi, err := os.Stat(lockFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return time.Since(fi.ModTime()) > maxAge, nil
}

// readPID reads the PID from the lock file.
func readPID(lockFile string) (int, error) {
	data, err := os.ReadFile(lockFile)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("failed to parse PID from lockfile: %v", err)
	}
	return pid, nil
}

// killProcess kills the process with the given PID.
func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// handleStaleLock checks for stale lock files. If the lock file is not stale or
// we can't read a valid PID from the lock file, then do nothing. If the PID in
// the lock file represents a still-running GooGet process, then kill the
// running GooGet. Remove the stale lock file in any case.
func handleStaleLock(lockFile string, maxAge time.Duration) {
	if stale, err := isLockFileStale(lockFile, maxAge); err != nil {
		logger.Errorf("Failed to check lock file staleness: %v", err)
		return
	} else if !stale {
		return
	}

	pid, err := readPID(lockFile)
	if err != nil {
		logger.Errorf("Failed to read PID from lock file: %v", err)
		return
	}

	if running, err := isGooGetRunning(pid); err != nil {
		logger.Errorf("Failed to check if PID %d is running: %v", pid, err)
	} else if running {
		fmt.Printf("GooGet lock held by stale process with PID %d, attempting to kill.\n", pid)
		if err := killProcess(pid); err != nil {
			fmt.Printf("Failed to kill process %d: %v\n", pid, err)
			return
		}
		fmt.Printf("Killed stale process with PID %d.\n", pid)
	}

	fmt.Println("Removing stale lock file.")
	var removeErr error
	// Retry removal since the OS might take a moment to release the file handle
	// after the process is killed.
	for i := 0; i < 5; i++ {
		removeErr = os.Remove(lockFile)
		if removeErr == nil || os.IsNotExist(removeErr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	logger.Errorf("Failed to remove stale lock file: %v", removeErr)
}

// ObtainLock attempts to obtain an exclusive lock on the provided file.
func ObtainLock(lockFile string, maxAge time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(lockFile), 0755); err != nil {
		return nil, err
	}

	handleStaleLock(lockFile, maxAge)

	f, err := os.OpenFile(lockFile, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}

	var cleanup func()
	c := make(chan error)
	go func() {
		cleanup, err = lock(f)
		c <- err
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// 90% of all GooGet runs happen in < 60s, we wait 70s.
	timeout := time.After(70 * time.Second)

	for {
		select {
		case err := <-c:
			if err != nil {
				return nil, fmt.Errorf("failed to obtain lock: %v", err)
			}
			return cleanup, nil
		case <-ticker.C:
			fmt.Println("GooGet lock already held, waiting...")
		case <-timeout:
			return nil, errors.New("timed out waiting for lock after 70 seconds")
		}
	}
}
