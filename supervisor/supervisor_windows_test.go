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

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procMessageBoxW = user32.NewProc("MessageBoxW")

func init() {
	platformHelpers["messagebox"] = func(args []string) {
		// Shows a blocking MessageBox, a standard #32770 dialog, like an unexpected prompt.
		text, _ := windows.UTF16PtrFromString("Setup needs your confirmation to continue.")
		caption, _ := windows.UTF16PtrFromString("Setup")
		procMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), 0)
	}
	platformHelpers["spawn_detached_child"] = func(args []string) {
		// Starts a stalled child with no inherited pipes, records its PID in args[0] and exits.
		child := spawnHelper("stall")
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start child: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(args[0], []byte(strconv.Itoa(child.Process.Pid)), 0644); err != nil {
			os.Exit(1)
		}
	}
}

// waitForExit waits until pid has exited or timeout elapses, and reports whether it exited.
func waitForExit(pid int, timeout time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// The process no longer exists.
		return true
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(timeout/time.Millisecond))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// readPID reads a PID written by a helper.
func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read PID file %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("Failed to parse PID %q: %v", data, err)
	}
	return pid
}

// killPID terminates pid, ignoring errors.
func killPID(pid int) {
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
		_ = windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
}

// TestWindows_GrandchildContainedAndKilled verifies that a grandchild started immediately is
// inside the job and is terminated with it, leaving no orphans.
func TestWindows_GrandchildContainedAndKilled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := helperCommand(t, "spawn_orphan_child", pidFile)
	opts := Options{InactivityTimeout: 2 * time.Second, pollInterval: 50 * time.Millisecond}

	if err := Run(cmd, opts, io.Discard); !errors.Is(err, ErrInactivityTimeout) {
		t.Fatalf("Run got error %v, want ErrInactivityTimeout", err)
	}
	child := readPID(t, pidFile)
	if !waitForExit(child, 5*time.Second) {
		killPID(child)
		t.Errorf("Grandchild %d survived the job termination", child)
	}
}

// TestWindows_MessageBoxAborts verifies that a persistent MessageBox with no progress aborts
// shortly after the grace period in unattended mode.
func TestWindows_MessageBoxAborts(t *testing.T) {
	cmd := helperCommand(t, "messagebox")
	grace := 2 * time.Second
	opts := Options{
		InactivityTimeout: -1,
		// HardTimeout bounds the test if the dialog is never detected.
		HardTimeout:    grace + 30*time.Second,
		UIGracePeriod:  grace,
		progressWindow: time.Second,
		pollInterval:   100 * time.Millisecond,
		Unattended:     true,
	}
	start := time.Now()
	err := Run(cmd, opts, io.Discard)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrInteractiveUIDetected) {
		t.Fatalf("Run got error %v, want ErrInteractiveUIDetected", err)
	}
	if !strings.Contains(err.Error(), `Class="#32770"`) {
		t.Errorf("Run error %q lacks the dialog class", err)
	}
	if elapsed < grace || elapsed > grace+15*time.Second {
		t.Errorf("Run took %v, want between %v and %v", elapsed, grace, grace+15*time.Second)
	}
}

// TestWindows_SuccessPathDetachedChildSurvives verifies that descendants survive a successful
// install because the job's kill-on-close limit is cleared.
func TestWindows_SuccessPathDetachedChildSurvives(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := helperCommand(t, "spawn_detached_child", pidFile)
	if err := Run(cmd, Options{InactivityTimeout: -1}, io.Discard); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	child := readPID(t, pidFile)
	defer killPID(child)
	if waitForExit(child, time.Second) {
		t.Errorf("Detached child %d was terminated after a successful install", child)
	}
}

// TestWindows_CallerCreationFlagsPreserved verifies that CREATE_SUSPENDED is merged into
// caller-provided creation flags and that the process still runs to completion.
func TestWindows_CallerCreationFlagsPreserved(t *testing.T) {
	cmd := helperCommand(t, "exit_code_42")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	err := Run(cmd, Options{InactivityTimeout: -1}, io.Discard)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("Run returned %v, want exit code 42", err)
	}
	want := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED)
	if cmd.SysProcAttr.CreationFlags&want != want {
		t.Errorf("CreationFlags got %#x, want both %#x", cmd.SysProcAttr.CreationFlags, want)
	}
}

// TestWindows_EnumContextRegistry verifies the integer-handle registry used by EnumWindows.
func TestWindows_EnumContextRegistry(t *testing.T) {
	ctx := &windowEnumContext{}
	h := registerEnumContext(ctx)
	if h == 0 || lookupEnumContext(h) != ctx {
		t.Fatalf("lookupEnumContext(%d) did not return the registered context", h)
	}
	unregisterEnumContext(h)
	if lookupEnumContext(h) != nil {
		t.Error("lookupEnumContext returned a context after unregister")
	}
	if enumWindowsProc(0, h) != 0 {
		t.Error("enumWindowsProc did not stop enumeration for an unknown handle")
	}
}

// TestWindows_IsCandidateWindow verifies the dialog candidate rules.
func TestWindows_IsCandidateWindow(t *testing.T) {
	for _, tc := range []struct {
		class    string
		hasOwner bool
		exStyle  uint32
		want     bool
	}{
		{"#32770", false, 0, true},
		{"WixBundleWindow", false, wsExDlgModalFrame, false},
		{"WixBundleWindow", true, 0, false},
		{"CustomModal", true, wsExDlgModalFrame, true},
	} {
		if got := isCandidateWindow(tc.class, tc.hasOwner, tc.exStyle); got != tc.want {
			t.Errorf("isCandidateWindow(%q, %v, %#x) got %v, want %v", tc.class, tc.hasOwner, tc.exStyle, got, tc.want)
		}
	}
}

// TestWindows_MutexExists verifies mutex detection using a test-local mutex name.
func TestWindows_MutexExists(t *testing.T) {
	mutexName := fmt.Sprintf(`Local\googet_supervisor_test_%d`, os.Getpid())
	if mutexExists(mutexName) {
		t.Fatal("mutexExists returned true before the mutex exists")
	}
	name, _ := windows.UTF16PtrFromString(mutexName)
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		t.Fatalf("CreateMutex failed: %v", err)
	}
	if !mutexExists(mutexName) {
		t.Error("mutexExists returned false while the mutex exists")
	}
	windows.CloseHandle(h)
	if mutexExists(mutexName) {
		t.Error("mutexExists returned true after the mutex was closed")
	}
}

// TestWindows_JobTreeRootExited verifies root exit detection through the process handle.
func TestWindows_JobTreeRootExited(t *testing.T) {
	cmd := helperCommand(t, "stall")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	proc, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("OpenProcess failed: %v", err)
	}
	defer windows.CloseHandle(proc)
	tree := &jobTree{c: cmd, proc: proc}
	if tree.rootExited() {
		t.Error("rootExited returned true for a running process")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !tree.rootExited() {
		t.Error("rootExited returned false after the process exited")
	}
}

// TestWindows_ResumeProcessThreadsUnknownPID verifies that resuming a nonexistent process fails.
func TestWindows_ResumeProcessThreadsUnknownPID(t *testing.T) {
	if err := resumeProcessThreads(0xFFFFFFF0); err == nil {
		t.Error("resumeProcessThreads succeeded for a nonexistent process")
	}
}

// TestWindows_NoJobFallbackEnforcesOnlyHardTimeout verifies that when the Job Object cannot be
// created, a stalled root process is not killed by the inactivity watchdog but still by the
// hard timeout.
func TestWindows_NoJobFallbackEnforcesOnlyHardTimeout(t *testing.T) {
	old := createJob
	createJob = func() (windows.Handle, error) { return 0, errors.New("injected job creation failure") }
	defer func() { createJob = old }()

	cmd := helperCommand(t, "stall")
	opts := Options{
		InactivityTimeout: 200 * time.Millisecond,
		HardTimeout:       time.Second,
		pollInterval:      20 * time.Millisecond,
	}
	if err := Run(cmd, opts, io.Discard); !errors.Is(err, ErrHardTimeout) {
		t.Fatalf("Run without a Job Object got error %v, want ErrHardTimeout", err)
	}
}
