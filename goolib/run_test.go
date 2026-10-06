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
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv selects what the test binary does when executed as a helper:
// "run" acts as googet, calling Run on a "hang" installer, and "run-stubborn"
// does the same with a "stubborn" one; "hang" starts a grandchild, prints its
// PID and hangs; "stubborn" does the same while ignoring SIGTERM; "exit" and
// "fail" do the same but exit at once with code 0 or 3; "sleep" is the
// grandchild; "spin" uses CPU without output for spinFor and exits.
const helperEnv = "GOOLIB_TEST_HELPER"

// spinFor is how long the "spin" helper runs.
const spinFor = 3 * time.Second

// helperCmd returns a command that runs the test binary in the given helper
// mode. The race detector's exit delay is disabled so helpers exit promptly.
func helperCmd(mode string) *exec.Cmd {
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), helperEnv+"="+mode, "GORACE=atexit_sleep_ms=0")
	return c
}

func TestMain(m *testing.M) {
	switch mode := os.Getenv(helperEnv); mode {
	case "run":
		Run(helperCmd("hang"), nil, io.Discard)
		os.Exit(0)
	case "run-stubborn":
		Run(helperCmd("stubborn"), nil, io.Discard)
		os.Exit(0)
	case "hang", "stubborn", "exit", "fail":
		if mode == "stubborn" {
			// The grandchild inherits the ignored disposition.
			signal.Ignore(syscall.SIGTERM)
		}
		// The grandchild inherits stdout, so it holds Run's pipe open too.
		gc := helperCmd("sleep")
		gc.Stdout = os.Stdout
		if err := gc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(gc.Process.Pid)
		switch mode {
		case "exit":
			os.Exit(0)
		case "fail":
			os.Exit(3)
		}
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	case "sleep":
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	case "spin":
		for end := time.Now().Add(spinFor); time.Now().Before(end); {
			// Burn CPU without output.
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runHelper runs the test binary in the given helper mode through Run with
// the given timeout and accepted exit codes. It returns the PID of the
// grandchild, which is killed at the end of the test if still running, how
// long Run took and its error.
func runHelper(t *testing.T, mode string, timeout time.Duration, ec []int) (pid int, elapsed time.Duration, err error) {
	t.Helper()
	orig := Timeout
	t.Cleanup(func() { Timeout = orig })
	Timeout = timeout
	var w syncBuffer
	start := time.Now()
	err = Run(helperCmd(mode), ec, &w)
	elapsed = time.Since(start)
	pid, perr := strconv.Atoi(strings.TrimSpace(w.String()))
	if perr != nil {
		t.Fatalf("Run(%s helper) printed %q, want a grandchild PID (Run error: %v)", mode, w.String(), err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil && processAlive(pid) {
			p.Kill()
		}
	})
	return pid, elapsed, err
}

// setWaitDelay sets waitDelay to d for the duration of the test.
func setWaitDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := waitDelay
	t.Cleanup(func() { waitDelay = orig })
	waitDelay = d
}

// waitGone reports whether process pid exits within d.
func waitGone(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); processAlive(pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

func TestRunTimeoutKillsTree(t *testing.T) {
	const timeout = 3 * time.Second
	pid, elapsed, err := runHelper(t, "hang", timeout, nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run() error = %v, want %v", err, ErrTimeout)
	}
	// A grandchild left holding the output pipe would delay Run by waitDelay.
	if elapsed > timeout+5*time.Second {
		t.Errorf("Run() took %v, want it to return promptly after the %v timeout", elapsed, timeout)
	}
	if !waitGone(pid, 5*time.Second) {
		t.Errorf("grandchild %d is still running after the timeout", pid)
	}
}

func TestRunNoTimeoutLeavesDescendants(t *testing.T) {
	setWaitDelay(t, 200*time.Millisecond)
	// The grandchild keeps the output pipe open after the command exits, which
	// must not block Run or count as a failure.
	pid, elapsed, err := runHelper(t, "exit", 0, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Run() took %v, want it bounded by waitDelay", elapsed)
	}
	// Descendants that outlive a normal exit keep running, as before.
	if !processAlive(pid) {
		t.Errorf("grandchild %d was killed after a normal exit", pid)
	}
}

func TestRunTimeoutWhileDrainingOutput(t *testing.T) {
	// The command exits at once, but the timeout fires while Run still waits
	// for the grandchild to close the output pipe.
	setWaitDelay(t, 2*time.Second)
	pid, elapsed, err := runHelper(t, "exit", time.Second, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if elapsed < time.Second {
		t.Errorf("Run() returned after %v, before the timeout fired", elapsed)
	}
	if !processAlive(pid) {
		t.Errorf("grandchild %d was killed although the command exited before the timeout", pid)
	}
}

func TestRunExitCodeWhileDrainingOutput(t *testing.T) {
	setWaitDelay(t, 200*time.Millisecond)
	for _, tt := range []struct {
		name    string
		ec      []int
		wantErr string
	}{
		{"rejected", nil, "command exited with error code 3"},
		{"accepted", []int{3}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := runHelper(t, "fail", time.Hour, tt.ec)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tt.wantErr {
				t.Errorf("Run() error = %q, want %q", got, tt.wantErr)
			}
		})
	}
}
