/*
Copyright 2026 Google Inc. All Rights Reserved.
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
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/supervisor"
)

// TestSupervisorOptions verifies that per-command overrides are mapped and that invalid
// overrides fall back to zero options.
func TestSupervisorOptions(t *testing.T) {
	got := supervisorOptions(goolib.ExecFile{Path: "install.sh", Timeout: "3h", InactivityTimeout: "0"})
	if got.HardTimeout != 3*time.Hour || got.InactivityTimeout != -1 {
		t.Errorf("supervisorOptions(valid) = %+v, want HardTimeout 3h and InactivityTimeout -1", got)
	}
	got = supervisorOptions(goolib.ExecFile{Path: "install.sh", Timeout: "forever"})
	if got.HardTimeout != 0 || got.InactivityTimeout != 0 || len(got.LogFiles) != 0 {
		t.Errorf("supervisorOptions(invalid) = %+v, want zero options", got)
	}
}

// writeSleepScript writes an executable shell script that sleeps for a long time.
func writeSleepScript(t *testing.T, dir, name string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Shell script execution is only exercised on Linux")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh is unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nsleep 30\n"), 0755); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
}

// TestVerifyAppliesTimeoutOverride verifies that Verify passes the per-command Timeout to
// the supervisor instead of running with the process-wide defaults.
func TestVerifyAppliesTimeoutOverride(t *testing.T) {
	dir := t.TempDir()
	writeSleepScript(t, dir, "verify.sh")
	ps := &goolib.PkgSpec{Name: "foo", Verify: goolib.ExecFile{Path: "verify.sh", Timeout: "500ms"}}

	start := time.Now()
	err := Verify(dir, ps)
	if !errors.Is(err, supervisor.ErrHardTimeout) {
		t.Fatalf("Verify() = %v, want an error wrapping ErrHardTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("Verify() took %v, want the 500ms override to terminate it early", elapsed)
	}
}

// TestInstallAppliesTimeoutOverride verifies that Install passes the per-command Timeout to
// the supervisor instead of running with the process-wide defaults.
func TestInstallAppliesTimeoutOverride(t *testing.T) {
	dir := t.TempDir()
	writeSleepScript(t, dir, "install.sh")
	ps := &goolib.PkgSpec{Name: "foo", Install: goolib.ExecFile{Path: "install.sh", Timeout: "500ms"}}

	start := time.Now()
	err := Install(dir, ps)
	if !errors.Is(err, supervisor.ErrHardTimeout) {
		t.Fatalf("Install() = %v, want an error wrapping ErrHardTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("Install() took %v, want the 500ms override to terminate it early", elapsed)
	}
}
