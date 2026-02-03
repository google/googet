//go:build darwin
// +build darwin

/*
Copyright 2016 Google Inc. All Rights Reserved.
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

package system

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// Install performs a system specfic install given a package extraction directory and a PkgSpec struct.
func Install(dir string, ps *goolib.PkgSpec) error {
	return nil
}

// Uninstall performs a system specfic uninstall given a package extraction directory and a PkgSpec struct.
func Uninstall(dir string, ps *client.PackageState) error {
	return nil
}

// InstallableArchs returns a slice of archs supported by this machine.
func InstallableArchs() ([]string, error) {
	return []string{"noarch", "x86_64", "arm64"}, nil
}

// AppAssociation returns empty strings and is a stub of the Windows implementation.
func AppAssociation(ps *goolib.PkgSpec, installSource string) (string, string) {
	return "", ""
}

// IsAdmin returns nil and is a stub of the Windows implementation
func IsAdmin() error {
	return nil
}

// isGooGetRunning checks if the process with the given PID is running and is a googet process.
func isGooGetRunning(pid int) (bool, error) {
	// Stub for Darwin, assuming not running to avoid complexity with ps or sysctl
	return false, nil
}

// lock attempts to obtain an exclusive lock on the provided file.
func lock(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	cleanup := func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		os.Remove(f.Name())
	}

	if err := f.Truncate(0); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to truncate lockfile: %v", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write PID to lockfile: %v", err)
	}

	// Downgrade to shared lock so that other processes can read the PID.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to downgrade to shared lock: %v", err)
	}
	return cleanup, nil
}
