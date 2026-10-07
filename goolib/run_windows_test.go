//go:build windows

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
	"io"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// processAlive reports whether pid exists and has not exited.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

func TestRunInactivity(t *testing.T) {
	const limit = spinFor / 3
	origMinInactivity, origMinInterval := minInactivity, minInterval
	t.Cleanup(func() { minInactivity, minInterval = origMinInactivity, origMinInterval })
	minInactivity, minInterval = 0, 100*time.Millisecond
	for _, tt := range []struct {
		name    string
		mode    string
		wantErr error
	}{
		// The helper sleeps without output, CPU time or I/O.
		{"inactive", "sleep", ErrInactive},
		// The helper writes nothing for three times the limit but uses CPU.
		{"busy", "spin", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			origTimeout, origLimit, origMode := Timeout, InactivityTimeout, InactivityMode
			t.Cleanup(func() { Timeout, InactivityTimeout, InactivityMode = origTimeout, origLimit, origMode })
			// The timeout is long enough for the spin helper to finish.
			Timeout, InactivityTimeout, InactivityMode = spinFor+4*time.Second, limit, InactivityEnforce
			if err := Run(helperCmd(tt.mode), nil, io.Discard); !errors.Is(err, tt.wantErr) {
				t.Errorf("Run(%s helper) error = %v, want %v", tt.mode, err, tt.wantErr)
			}
		})
	}
}
