//go:build !windows

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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	platformHelpers["setsid_grandchild"] = func(args []string) {
		// Starts a grandchild in a new session that inherits stdout, records its PID in args[0],
		// and then either exits (args[1] == "exit") or stalls.
		gc := spawnHelper("stall")
		gc.Stdout = os.Stdout
		gc.Stderr = os.Stderr
		gc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := gc.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start grandchild: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(args[0], []byte(strconv.Itoa(gc.Process.Pid)), 0644); err != nil {
			os.Exit(1)
		}
		if len(args) > 1 && args[1] == "exit" {
			return
		}
		time.Sleep(60 * time.Second)
	}
}

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid int) bool {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		if i := strings.LastIndexByte(string(data), ')'); i >= 0 && i+2 < len(data) {
			return data[i+2] != 'Z'
		}
		return true
	}
	return syscall.Kill(pid, 0) == nil
}

// waitForExit polls until pid has exited or timeout elapses, and reports whether it exited.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !processAlive(pid)
}

// readPIDFile waits for a helper to write a PID to path and returns it.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Helper did not write a PID to %s", path)
	return 0
}

// killOnCleanup kills the process recorded in pidFile when the test ends.
func killOnCleanup(t *testing.T, pidFile string) {
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

// TestParseProcStat verifies stat parsing with spaces and parentheses in the command name.
func TestParseProcStat(t *testing.T) {
	line := "1234 (my (odd) proc) S 1 777 777 0 -1 4194560 100 0 0 0 150 50 0 0 20 0 1 0 100 0 0\n"
	st, ok := parseProcStat([]byte(line))
	if !ok {
		t.Fatal("parseProcStat failed on a valid line")
	}
	if st.pgrp != 777 {
		t.Errorf("pgrp got %d, want 777", st.pgrp)
	}
	if st.state != 'S' {
		t.Errorf("state got %q, want 'S'", st.state)
	}
	if st.cpu != 2*time.Second {
		t.Errorf("cpu got %v, want 2s from 150+50 ticks", st.cpu)
	}
	if st.start != 100 {
		t.Errorf("start got %d, want 100", st.start)
	}
	if short, ok := parseProcStat([]byte("1 (p) S 1 1 1 0 -1 0 0 0 0 0 7 3\n")); !ok || short.start != 0 {
		t.Errorf("parseProcStat(short line) = %+v, %v; want start 0 and ok", short, ok)
	}
	if _, ok := parseProcStat([]byte("garbage")); ok {
		t.Error("parseProcStat accepted garbage")
	}
}

// TestParseProcIO verifies that only storage read and write bytes are summed.
func TestParseProcIO(t *testing.T) {
	data := "rchar: 999\nwchar: 999\nsyscr: 1\nsyscw: 1\nread_bytes: 4096\nwrite_bytes: 8192\ncancelled_write_bytes: 0\n"
	if got := parseProcIO([]byte(data)); got != 12288 {
		t.Errorf("parseProcIO got %d, want 12288", got)
	}
}

// byPID re-keys per-process counters by PID alone.
func byPID(m map[procKey]counters) map[uint32]counters {
	out := make(map[uint32]counters, len(m))
	for k, c := range m {
		out[k.pid] = c
	}
	return out
}

// TestSampleProcessGroup verifies pgid filtering and root fallback against a fake procfs.
func TestSampleProcessGroup(t *testing.T) {
	root := t.TempDir()
	write := func(pid, pgrp, utime int, io string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (p) S 1 %d %d 0 -1 0 0 0 0 0 %d 0 0 0 20 0 1 0 0 0 0\n", pid, pgrp, pgrp, utime)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0644); err != nil {
			t.Fatal(err)
		}
		if io != "" {
			if err := os.WriteFile(filepath.Join(dir, "io"), []byte(io), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(10, 10, 100, "read_bytes: 1\nwrite_bytes: 2\n")
	write(11, 10, 50, "")
	write(12, 99, 500, "read_bytes: 1000\n")
	if err := os.MkdirAll(filepath.Join(root, "self"), 0755); err != nil {
		t.Fatal(err)
	}

	old := procRoot
	procRoot = root
	defer func() { procRoot = old }()

	got := byPID(sampleProcessGroup(10, 10))
	if len(got) != 2 || got[10].cpu != time.Second || got[10].io != 3 || got[11].cpu != 500*time.Millisecond {
		t.Errorf("sampleProcessGroup got %+v, want pids 10 and 11 only", got)
	}

	procRoot = filepath.Join(root, "missing")
	if got := sampleProcessGroup(10, 10); len(got) != 0 {
		t.Errorf("sampleProcessGroup without procfs got %+v, want empty", got)
	}
}

// TestRun_PgidAggregation verifies that CPU burned by a child keeps the tree alive while the
// root process only sleeps.
func TestRun_PgidAggregation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Process group accounting requires /proc")
	}
	cmd := helperCommand(t, "parent_sleep_child_burn", "2500")
	opts := Options{
		InactivityTimeout: 1 * time.Second,
		pollInterval:      20 * time.Millisecond,
		minCPUDelta:       50 * time.Millisecond,
	}
	start := time.Now()
	if err := Run(cmd, opts, io.Discard); err != nil {
		t.Fatalf("Run returned %v while a child was burning CPU, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 2300*time.Millisecond {
		t.Errorf("Run completed in %v, want at least the child's 2.5s of work", elapsed)
	}
}

// TestRun_SysProcAttrMerged verifies that Setpgid is merged into a caller-provided SysProcAttr.
func TestRun_SysProcAttrMerged(t *testing.T) {
	cmd := helperCommand(t, "sleep_ms", "10")
	attr := &syscall.SysProcAttr{Setpgid: false}
	cmd.SysProcAttr = attr
	if err := Run(cmd, Options{InactivityTimeout: -1}, io.Discard); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if cmd.SysProcAttr != attr || !attr.Setpgid {
		t.Errorf("SysProcAttr got %+v (same pointer: %v), want the caller's struct with Setpgid set", cmd.SysProcAttr, cmd.SysProcAttr == attr)
	}
}

// TestRun_BoundedWait_WaitDelay verifies that a grandchild outside the process group holding
// stdout cannot block Run after an abort.
func TestRun_BoundedWait_WaitDelay(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	killOnCleanup(t, pidFile)
	cmd := helperCommand(t, "setsid_grandchild", pidFile)
	cmd.WaitDelay = 500 * time.Millisecond
	opts := Options{InactivityTimeout: 500 * time.Millisecond, pollInterval: 20 * time.Millisecond}

	start := time.Now()
	err := Run(cmd, opts, io.Discard)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrInactivityTimeout) {
		t.Fatalf("Run got error %v, want ErrInactivityTimeout", err)
	}
	if elapsed > 20*time.Second {
		t.Errorf("Run took %v, want it bounded by WaitDelay", elapsed)
	}
	if pid := readPIDFile(t, pidFile); !processAlive(pid) {
		t.Errorf("Grandchild %d exited early; the held-pipe scenario was not exercised", pid)
	}
}

// TestRun_BoundedWait_KillWaitTimeout verifies the last-resort bound on Wait after an abort.
func TestRun_BoundedWait_KillWaitTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	killOnCleanup(t, pidFile)
	old := killWaitTimeout
	killWaitTimeout = 500 * time.Millisecond
	defer func() { killWaitTimeout = old }()

	cmd := helperCommand(t, "setsid_grandchild", pidFile)
	cmd.WaitDelay = time.Hour
	opts := Options{InactivityTimeout: 500 * time.Millisecond, pollInterval: 20 * time.Millisecond}

	start := time.Now()
	err := Run(cmd, opts, io.Discard)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrInactivityTimeout) {
		t.Fatalf("Run got error %v, want ErrInactivityTimeout", err)
	}
	if !strings.Contains(err.Error(), "pipes are held by processes outside the supervised tree") {
		t.Errorf("Run error %q lacks the held-pipe diagnostic", err)
	}
	if elapsed > 20*time.Second {
		t.Errorf("Run took %v, want it bounded by killWaitTimeout", elapsed)
	}
}

// TestRun_SuccessWithHeldPipe verifies that a clean exit is not reported as an error when a
// detached grandchild keeps stdout open past WaitDelay.
func TestRun_SuccessWithHeldPipe(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	killOnCleanup(t, pidFile)
	cmd := helperCommand(t, "setsid_grandchild", pidFile, "exit")
	cmd.WaitDelay = 300 * time.Millisecond
	start := time.Now()
	if err := Run(cmd, Options{InactivityTimeout: -1}, io.Discard); err != nil {
		t.Fatalf("Run returned %v, want nil after a clean exit", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("Run took %v, want it bounded by WaitDelay", elapsed)
	}
}

// TestProcExited verifies zombie and reaped detection against a fake procfs.
func TestProcExited(t *testing.T) {
	root := t.TempDir()
	write := func(pid int, state string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (p) %s 1 %d %d 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 0 0 0\n", pid, state, pid, pid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(10, "S")
	write(11, "Z")

	old := procRoot
	procRoot = root
	defer func() { procRoot = old }()

	for _, tc := range []struct {
		name        string
		pid         int
		procfsWorks bool
		want        bool
	}{
		{"Running", 10, true, false},
		{"Zombie", 11, true, true},
		{"Reaped", 12, true, true},
		{"NoProcfs", 12, false, false},
	} {
		if got := procExited(tc.pid, tc.procfsWorks); got != tc.want {
			t.Errorf("%s: procExited(%d, %v) got %v, want %v", tc.name, tc.pid, tc.procfsWorks, got, tc.want)
		}
	}
}

// TestRun_RootExitedWithHeldPipeNotKilled verifies that after the root process exits cleanly, a
// detached grandchild holding stdout does not turn the install into a watchdog failure.
func TestRun_RootExitedWithHeldPipeNotKilled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Root exit detection requires /proc")
	}
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	killOnCleanup(t, pidFile)
	cmd := helperCommand(t, "setsid_grandchild", pidFile, "exit")
	cmd.WaitDelay = 3 * time.Second
	opts := Options{InactivityTimeout: time.Second, pollInterval: 20 * time.Millisecond}
	if err := Run(cmd, opts, io.Discard); err != nil {
		t.Fatalf("Run returned %v, want nil because the root exited cleanly before the inactivity timeout", err)
	}
	if pid := readPIDFile(t, pidFile); !processAlive(pid) {
		t.Errorf("Grandchild %d exited early; the held-pipe scenario was not exercised", pid)
	}
}
