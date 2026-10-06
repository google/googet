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
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// parentPID returns the parent PID of pid from a Toolhelp process snapshot.
func parentPID(t *testing.T, pid int) int {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatalf("CreateToolhelp32Snapshot() error = %v", err)
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == uint32(pid) {
			return int(e.ParentProcessID)
		}
	}
	t.Fatalf("process %d not found in Toolhelp snapshot (last error: %v)", pid, err)
	return 0
}

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

func TestRunKillsTreeWhenGoogetDies(t *testing.T) {
	// The helper plays googet: it calls Run on an installer that starts a
	// grandchild and hangs. Killing the helper closes its handle to the job.
	h, pid := startGooget(t, "run", nil, nil)
	// A handle opened while the grandchild is known to be alive cannot refer
	// to another process later.
	gc, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess(%d) error = %v", pid, err)
	}
	defer windows.CloseHandle(gc)
	if err := h.Process.Kill(); err != nil {
		t.Fatalf("killing helper: %v", err)
	}
	h.Wait()
	if ev, err := windows.WaitForSingleObject(gc, 5000); err != nil || ev != windows.WAIT_OBJECT_0 {
		windows.TerminateProcess(gc, 1)
		t.Errorf("grandchild %d still running 5s after the helper was killed (wait = %v, %v)", pid, ev, err)
	}
}

func TestRunCtrlBreak(t *testing.T) {
	// The helper plays googet (see actAsGooget) in a new process group, the
	// only one the event is sent to; the grandchild inherits that group and
	// survives the event, as a GUI child does, so only Run terminating the job
	// stops it. "stubborn" makes the installer survive the event too; "console"
	// and "console-detached" (without stdout inherited by the grandchild) let
	// the event end the installer with STATUS_CONTROL_C_EXIT before the relay
	// runs, as a console installer does. Ctrl-C is disabled in a new
	// process group, so the test sends Ctrl-Break, which Go also reports as
	// os.Interrupt.
	for _, tt := range []struct {
		name, mode string
	}{
		{"stubborn", "run-stubborn"},
		{"console", "run-stubborn-child"},
		{"console-detached", "run-stubborn-detached"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stderr syncBuffer
			h, pid := startGooget(t, tt.mode, &stderr, &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP})
			gc, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
			if err != nil {
				t.Fatalf("OpenProcess(%d) error = %v", pid, err)
			}
			defer windows.CloseHandle(gc)
			if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(h.Process.Pid)); err != nil {
				windows.TerminateProcess(gc, 1)
				t.Skipf("cannot send CTRL_BREAK_EVENT, likely because the test has no console: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- h.Wait() }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				h.Process.Kill()
				<-done
				t.Fatalf("helper still running 10s after CTRL_BREAK_EVENT\nstderr:\n%s", stderr.String())
			}
			if !strings.Contains(stderr.String(), runReturned) {
				t.Errorf("helper stderr = %q, want %q", stderr.String(), runReturned)
			}
			if got, want := uint32(h.ProcessState.ExitCode()), uint32(windows.STATUS_CONTROL_C_EXIT); got != want {
				t.Errorf("helper exit code = %#x, want %#x", got, want)
			}
			if ev, err := windows.WaitForSingleObject(gc, 5000); err != nil || ev != windows.WAIT_OBJECT_0 {
				windows.TerminateProcess(gc, 1)
				t.Errorf("grandchild %d still running 5s after CTRL_BREAK_EVENT (wait = %v, %v)", pid, ev, err)
			}
		})
	}
}

func TestRunWithoutJob(t *testing.T) {
	// Not parallel, since it replaces newJobFunc for the whole package.
	orig := newJobFunc
	t.Cleanup(func() { newJobFunc = orig })
	newJobFunc = func(windows.Handle) (windows.Handle, error) {
		return 0, errors.New("job objects unavailable")
	}
	// The surviving grandchild holds the output pipe open.
	setWaitDelay(t, 200*time.Millisecond)
	pid, _, err := runHelper(t, "hang", 3*time.Second, nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run() error = %v, want %v", err, ErrTimeout)
	}
	// Without a job only the command is killed; runHelper kills the grandchild
	// at the end of the test.
	if !processAlive(pid) {
		t.Errorf("grandchild %d was killed, want only the command killed without a job", pid)
	}
	// Nor is there an activity counter for the inactivity limit.
	c := helperCmd("true")
	sigs := catchSignals()
	p, err := startContained(c, sigs)
	if err != nil {
		sigs.stop()
		t.Fatalf("startContained() error = %v", err)
	}
	c.Wait()
	sigs.stop()
	p.release()
	if p.activity != nil {
		t.Error("startContained() activity != nil without a job, want nil")
	}
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

func TestRunCtrlBreakAfterExit(t *testing.T) {
	// The helper plays googet (see actAsGooget) in a new process group with
	// an installer that exits at once, leaving a grandchild that survives the
	// event but holds the output open. An event that arrives once the
	// installer has exited must neither kill the grandchild nor undo the
	// installer's success, but must still stop googet.
	var stderr syncBuffer
	h, pid := startGooget(t, "run-exit-stubborn", &stderr, &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP})
	gc, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess(%d) error = %v", pid, err)
	}
	defer windows.CloseHandle(gc)
	defer windows.TerminateProcess(gc, 1)
	// startContained holds a handle to the installer until the 5s waitDelay
	// ends, so its PID stays valid and waitGone observes its exit.
	installerPID := parentPID(t, pid)
	if !waitGone(installerPID, 5*time.Second) {
		t.Fatalf("installer %d is still running", installerPID)
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(h.Process.Pid)); err != nil {
		t.Skipf("cannot send CTRL_BREAK_EVENT, likely because the test has no console: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		h.Process.Kill()
		<-done
		t.Fatalf("helper still running 10s after CTRL_BREAK_EVENT\nstderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), runReturned) {
		t.Errorf("helper stderr = %q, want %q", stderr.String(), runReturned)
	}
	if got, want := uint32(h.ProcessState.ExitCode()), uint32(windows.STATUS_CONTROL_C_EXIT); got != want {
		t.Errorf("helper exit code = %#x, want %#x", got, want)
	}
	if ev, err := windows.WaitForSingleObject(gc, 0); err != nil || ev != uint32(windows.WAIT_TIMEOUT) {
		t.Errorf("grandchild %d was killed by an event that came after the installer exited (wait = %v, %v)", pid, ev, err)
	}
}
