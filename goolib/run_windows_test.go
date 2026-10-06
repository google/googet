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
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

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

// dialogTitle is the title of the dialogs the "dialog" and "hidden-dialog"
// helpers show.
const dialogTitle = "googet test dialog"

func init() {
	s, _ := windows.UTF16PtrFromString(dialogTitle)
	switch os.Getenv(helperEnv) {
	case "dialog":
		// Show a message box and block until it is closed.
		windows.MessageBox(0, s, s, windows.MB_OK)
		os.Exit(0)
	case "hidden-dialog":
		// Create a standard dialog without WS_VISIBLE, like a hidden helper
		// window, and keep it, which needs no message loop.
		runtime.LockOSThread()
		class, _ := windows.UTF16PtrFromString(dialogClass)
		windows.NewLazySystemDLL("user32.dll").NewProc("CreateWindowExW").Call(
			0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(s)), 0, 0, 0, 0, 0, 0, 0, 0, 0)
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
}

func TestRunDialog(t *testing.T) {
	origTimeout, origLimit, origMode, origGrace := Timeout, InactivityTimeout, InactivityMode, DialogGrace
	origMinInactivity, origIsUnattended := minInactivity, isUnattended
	t.Cleanup(func() {
		Timeout, InactivityTimeout, InactivityMode, DialogGrace = origTimeout, origLimit, origMode, origGrace
		minInactivity, isUnattended = origMinInactivity, origIsUnattended
	})
	// Dialogs are sampled every 10s/6 and acted on within about 5s, before
	// the helper can become inactive at 10s. Whether this runs in session 0
	// varies, so nobody answering is forced.
	InactivityTimeout, InactivityMode, DialogGrace = 10*time.Second, InactivityEnforce, time.Second
	minInactivity, isUnattended = 0, func() bool { return true }
	for _, tt := range []struct {
		mode    string
		timeout time.Duration
		wantErr error
	}{
		{"dialog", 30 * time.Second, ErrDialog},
		// A hidden dialog is not a dialog, so the timeout comes first.
		{"hidden-dialog", 8 * time.Second, ErrTimeout},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			Timeout = tt.timeout
			err := Run(helperCmd(tt.mode), nil, io.Discard)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Run(%s helper) error = %v, want %v", tt.mode, err, tt.wantErr)
			}
			if tt.wantErr == ErrDialog && (err == nil || !strings.Contains(err.Error(), dialogTitle)) {
				t.Errorf("Run(%s helper) error = %v, want the title %q", tt.mode, err, dialogTitle)
			}
		})
	}
}

func TestUnattended(t *testing.T) {
	if _, err := unattended(); err != nil {
		t.Errorf("unattended() error = %v, want nil", err)
	}
}
