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

package goolib

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/google/googet/v2/supervisor"
)

// exitError returns a real *exec.ExitError for a process that exited with code.
func exitError(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/c", fmt.Sprintf("exit %d", code))
	} else {
		c = exec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
	}
	err := c.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Skipf("Could not produce an exit error: %v", err)
	}
	return ee
}

// TestCheckExit verifies how supervisor.Run results map to RunWithOptions errors.
func TestCheckExit(t *testing.T) {
	ee := exitError(t, 3)
	startErr := errors.New("start failed")
	tests := []struct {
		name    string
		err     error
		ec      []int
		wantNil bool
	}{
		{name: "success", err: nil, wantNil: true},
		{name: "accepted exit code", err: ee, ec: []int{3}, wantNil: true},
		{name: "unaccepted exit code", err: ee, ec: []int{1}},
		{name: "non-exit error", err: startErr, ec: []int{3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkExit(tc.err, tc.ec)
			if (got == nil) != tc.wantNil {
				t.Errorf("checkExit(%v, %v) = %v, want nil: %v", tc.err, tc.ec, got, tc.wantNil)
			}
		})
	}
	if got := checkExit(startErr, []int{3}); got != startErr {
		t.Errorf("checkExit(%v, [3]) = %v, want it returned unchanged", startErr, got)
	}
}

// TestCheckExitTerminatedIsNotAccepted verifies that a supervisor termination is returned
// unchanged even when the killed process's exit code is listed as acceptable.
func TestCheckExitTerminatedIsNotAccepted(t *testing.T) {
	ee := exitError(t, 3)
	terminated := fmt.Errorf("%w: %w", supervisor.ErrHardTimeout, ee)
	got := checkExit(terminated, []int{3, 1, -1, 137})
	if got != terminated {
		t.Fatalf("checkExit(terminated, ec) = %v, want %v", got, terminated)
	}
	if !errors.Is(got, supervisor.ErrTerminated) {
		t.Errorf("errors.Is(%v, ErrTerminated) = false, want true", got)
	}
}

// TestExecFileSupervisorOptions verifies the mapping of ExecFile overrides to supervisor options.
func TestExecFileSupervisorOptions(t *testing.T) {
	tests := []struct {
		name    string
		ef      ExecFile
		want    supervisor.Options
		wantErr bool
	}{
		{name: "no overrides", ef: ExecFile{Path: "install.cmd"}, want: supervisor.Options{}},
		{name: "explicit values", ef: ExecFile{Timeout: "3h", InactivityTimeout: "20m"}, want: supervisor.Options{HardTimeout: 3 * time.Hour, InactivityTimeout: 20 * time.Minute}},
		{name: "zero disables", ef: ExecFile{Timeout: "0", InactivityTimeout: "0s"}, want: supervisor.Options{HardTimeout: -1, InactivityTimeout: -1}},
		{name: "negative rejected", ef: ExecFile{Timeout: "-1h"}, wantErr: true},
		{name: "garbage rejected", ef: ExecFile{InactivityTimeout: "soon"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.ef.SupervisorOptions()
			if (err != nil) != tc.wantErr {
				t.Fatalf("SupervisorOptions() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if got.HardTimeout != 0 || got.InactivityTimeout != 0 || got.Mode != supervisor.ModeUnset {
					t.Errorf("SupervisorOptions() on error = %+v, want zero options", got)
				}
				return
			}
			if got.HardTimeout != tc.want.HardTimeout || got.InactivityTimeout != tc.want.InactivityTimeout || got.Mode != supervisor.ModeUnset || got.Unattended || got.DisableUIDetection || got.UIGracePeriod != 0 || len(got.LogFiles) != 0 {
				t.Errorf("SupervisorOptions() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
