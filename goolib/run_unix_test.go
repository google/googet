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
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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

func TestRunForwardsSignals(t *testing.T) {
	// The helper plays googet: it calls Run on an installer that starts a
	// grandchild and hangs. SIGTERM is used because SIGINT may be ignored when
	// tests run in the background, and Run leaves ignored signals alone.
	for _, tt := range []struct {
		name    string
		mode    string
		signals int
	}{
		{"forwarded", "run", 1},
		// The installer ignores SIGTERM, so only a second one stops it.
		{"escalated", "run-stubborn", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := helperCmd(tt.mode)
			out, err := h.StdoutPipe()
			if err != nil {
				t.Fatalf("StdoutPipe() error = %v", err)
			}
			if err := h.Start(); err != nil {
				t.Fatalf("starting helper: %v", err)
			}
			line, rerr := bufio.NewReader(out).ReadString('\n')
			pid, perr := strconv.Atoi(strings.TrimSpace(line))
			if rerr != nil || perr != nil {
				h.Process.Kill()
				h.Wait()
				t.Fatalf("helper printed %q (read error: %v), want a grandchild PID", line, rerr)
			}
			// The grandchild is in the installer's group, which is recorded while
			// it is known to be alive.
			pgid, gerr := syscall.Getpgid(pid)
			if gerr != nil || pgid == syscall.Getpgrp() {
				h.Process.Kill()
				h.Wait()
				t.Fatalf("Getpgid(%d) = %d, %v, want the installer's own group", pid, pgid, gerr)
			}
			t.Cleanup(func() {
				// Kill the installer's whole group if forwarding failed to.
				syscall.Kill(-pgid, syscall.SIGKILL)
			})
			done := make(chan error, 1)
			go func() { done <- h.Wait() }()
			for i := 1; i < tt.signals; i++ {
				h.Process.Signal(syscall.SIGTERM)
				// Pausing keeps the signals from coalescing and checks that an
				// ignored signal does not end the run.
				select {
				case err := <-done:
					t.Fatalf("helper exited after SIGTERM %d of %d: %v", i, tt.signals, err)
				case <-time.After(300 * time.Millisecond):
				}
			}
			if err := h.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("signaling helper: %v", err)
			}
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				h.Process.Kill()
				<-done
				t.Fatal("helper still running 10s after the last SIGTERM")
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("helper Wait() error = %v, want it killed by SIGTERM", err)
			}
			if ws := ee.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
				t.Errorf("helper exit status = %v, want killed by SIGTERM", ee)
			}
			if !waitGone(pid, 5*time.Second) {
				t.Errorf("grandchild %d is still running after SIGTERM", pid)
			}
		})
	}
}
