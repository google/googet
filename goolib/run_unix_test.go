//go:build unix

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
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processAlive reports whether pid exists and, where procfs is available, is
// not a zombie waiting to be reaped.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// The state follows the parenthesized command name.
	i := bytes.LastIndexByte(b, ')')
	return i < 0 || i+2 >= len(b) || b[i+2] != 'Z'
}

// waitReaped reports whether pid has been reaped (ESRCH, matching exited in
// run_unix.go) within d.
func waitReaped(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); syscall.Kill(pid, 0) != syscall.ESRCH; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

func TestRunForwardsSignals(t *testing.T) {
	// The helper plays googet (see actAsGooget): it calls Run on an installer
	// that starts a grandchild and hangs, and calls Reraise once Run returns.
	// SIGTERM is used because SIGINT may be ignored when tests run in the
	// background, and Run leaves ignored signals alone.
	for _, tt := range []struct {
		name string
		mode string
		// All but the last signal must leave the helper and grandchild running.
		signals []syscall.Signal
	}{
		{"forwarded", "run", []syscall.Signal{syscall.SIGTERM}},
		{"quit", "run", []syscall.Signal{syscall.SIGQUIT}},
		// The installer and grandchild ignore SIGTERM, so only a second one
		// stops them.
		{"escalated", "run-stubborn", []syscall.Signal{syscall.SIGTERM, syscall.SIGTERM}},
		// The installer dies from the first SIGTERM while its stubborn
		// grandchild holds stdout open; a second SIGTERM still escalates.
		{"escalated after installer exits", "run-stubborn-child", []syscall.Signal{syscall.SIGTERM, syscall.SIGTERM}},
		// The helper ignores SIGHUP, as under nohup, so Run must not forward it
		// or count it as an interruption.
		{"ignored", "run-nohup", []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stderr syncBuffer
			h, pid := startGooget(t, tt.mode, &stderr, nil)
			// The grandchild is in the installer's group, which is recorded while
			// it is known to be alive.
			pgid, gerr := syscall.Getpgid(pid)
			if gerr != nil || pgid == syscall.Getpgrp() {
				t.Fatalf("Getpgid(%d) = %d, %v, want the installer's own group", pid, pgid, gerr)
			}
			t.Cleanup(func() {
				// Kill the installer's whole group if forwarding failed to.
				syscall.Kill(-pgid, syscall.SIGKILL)
			})
			done := make(chan error, 1)
			go func() { done <- h.Wait() }()
			last := tt.signals[len(tt.signals)-1]
			for i, s := range tt.signals[:len(tt.signals)-1] {
				h.Process.Signal(s)
				// Pausing keeps the signals from coalescing and checks that this
				// one does not end the run.
				select {
				case err := <-done:
					t.Fatalf("helper exited after signal %d (%v): %v\nstderr:\n%s", i+1, s, err, stderr.String())
				case <-time.After(300 * time.Millisecond):
				}
				if tt.mode == "run-stubborn-child" && !waitReaped(pgid, 5*time.Second) {
					t.Fatalf("installer %d is not yet reaped after signal %d (%v)", pgid, i+1, s)
				}
				if !processAlive(pid) {
					t.Fatalf("grandchild %d exited after signal %d (%v)", pid, i+1, s)
				}
			}
			if err := h.Process.Signal(last); err != nil {
				t.Fatalf("signaling helper: %v", err)
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				h.Process.Kill()
				<-done
				t.Fatalf("helper still running 10s after %v\nstderr:\n%s", last, stderr.String())
			}
			_, afterRun, ok := strings.Cut(stderr.String(), runReturned)
			if !ok {
				t.Errorf("helper stderr = %q, want %q before it died", stderr.String(), runReturned)
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("helper Wait() error = %v, want it to die from %v", err, last)
			}
			ws := ee.Sys().(syscall.WaitStatus)
			if last == syscall.SIGQUIT {
				// On SIGQUIT the Go runtime dumps its goroutines and exits with
				// status 2 rather than dying from the signal. Matching
				// actAsGooget after runReturned ensures the dump came from the
				// helper's Reraise rather than a child.
				if ws.Signaled() || ws.ExitStatus() != 2 || !strings.Contains(afterRun, "actAsGooget") {
					t.Errorf("helper exit status = %v, stderr after Run = %q, want 2 with an actAsGooget SIGQUIT dump", ee, afterRun)
				}
			} else if !ws.Signaled() || ws.Signal() != last {
				t.Errorf("helper exit status = %v, want killed by %v", ee, last)
			}
			if !waitGone(pid, 5*time.Second) {
				t.Errorf("grandchild %d is still running after %v", pid, last)
			}
		})
	}
}

func TestRunSignalAfterExit(t *testing.T) {
	// The helper plays googet (see actAsGooget) with an installer that exits
	// at once, leaving a grandchild that holds the output open. A signal that
	// arrives once the installer has exited must neither reach the grandchild
	// nor undo the installer's success, but must still stop googet.
	var stderr syncBuffer
	h, pid := startGooget(t, "run-exit", &stderr, nil)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	pgid, gerr := syscall.Getpgid(pid)
	if gerr != nil {
		t.Fatalf("Getpgid(%d) error = %v", pid, gerr)
	}
	done := make(chan error, 1)
	go func() { done <- h.Wait() }()
	// The installer is the group leader; wait until Run has reaped it, while
	// it still waits 5s for the grandchild.
	if !waitReaped(pgid, 5*time.Second) {
		t.Fatalf("installer %d is not yet reaped", pgid)
	}
	if err := h.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signaling helper: %v", err)
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		h.Process.Kill()
		<-done
		t.Fatalf("helper still running 10s after SIGTERM\nstderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), runReturned) {
		t.Errorf("helper stderr = %q, want %q", stderr.String(), runReturned)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || !ee.Sys().(syscall.WaitStatus).Signaled() || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Errorf("helper Wait() error = %v, want killed by SIGTERM", err)
	}
	if !processAlive(pid) {
		t.Errorf("grandchild %d was killed by a signal that came after the installer exited", pid)
	}
}
