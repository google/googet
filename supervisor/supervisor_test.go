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
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/logger"
)

// platformHelpers holds helper subcommands registered by platform-specific test files.
var platformHelpers = map[string]func(args []string){}

// helperCommand returns an *exec.Cmd configured to invoke TestHelperProcess.
func helperCommand(t *testing.T, subcmd string, extraArgs ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], helperArgs(subcmd, extraArgs...)...)
	cmd.Env = helperEnv()
	return cmd
}

// helperArgs returns the arguments that make the test binary run a helper subcommand.
func helperArgs(subcmd string, extraArgs ...string) []string {
	args := []string{"-test.run=^TestHelperProcess$", "--", subcmd}
	return append(args, extraArgs...)
}

// helperEnv returns the environment for helper processes.
//
// GORACE=atexit_sleep_ms=0 stops race-enabled helpers from idling for one second at exit, which
// would otherwise look like inactivity to the supervisor.
func helperEnv() []string {
	return append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "GORACE=atexit_sleep_ms=0")
}

// spawnHelper starts a helper subcommand from inside a helper process.
func spawnHelper(subcmd string, extraArgs ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], helperArgs(subcmd, extraArgs...)...)
	cmd.Env = helperEnv()
	return cmd
}

// burnCPU busy-loops for d of wall time.
func burnCPU(d time.Duration) {
	start := time.Now()
	for time.Since(start) < d {
		_ = strconv.Itoa(int(time.Now().UnixNano()))
	}
}

// appendLines appends n lines to path, sleeping interval before each.
func appendLines(path string, n int, interval time.Duration) {
	for i := 0; i < n; i++ {
		time.Sleep(interval)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open log file: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(f, "progress line %d\n", i)
		f.Close()
	}
}

// atoiOr parses s as an integer or returns def.
func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// TestHelperProcess acts as a mock subprocess for supervisor tests.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "No command provided to helper process.\n")
		os.Exit(2)
	}

	cmd, cmdArgs := args[0], args[1:]
	arg := func(i int) string {
		if i < len(cmdArgs) {
			return cmdArgs[i]
		}
		return ""
	}
	switch cmd {
	case "stall":
		// Sleeps without performing any I/O or consuming CPU.
		time.Sleep(60 * time.Second)
	case "sleep_ms":
		// Sleeps for the given number of milliseconds and exits successfully.
		time.Sleep(time.Duration(atoiOr(arg(0), 200)) * time.Millisecond)
	case "active_logging":
		// Appends arg(1) lines to log file arg(0), one every arg(2) milliseconds.
		appendLines(arg(0), atoiOr(arg(1), 15), time.Duration(atoiOr(arg(2), 100))*time.Millisecond)
	case "log_then_stall":
		// Appends arg(1) lines every arg(2) milliseconds, then stalls.
		appendLines(arg(0), atoiOr(arg(1), 10), time.Duration(atoiOr(arg(2), 50))*time.Millisecond)
		time.Sleep(60 * time.Second)
	case "burn_cpu":
		// Consumes CPU in a busy loop for arg(0) milliseconds.
		burnCPU(time.Duration(atoiOr(arg(0), 250)) * time.Millisecond)
	case "spawn_orphan_child":
		// Spawns a stalled child, records its PID in arg(0) and stalls.
		child := spawnHelper("stall")
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start child process: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(arg(0), []byte(strconv.Itoa(child.Process.Pid)), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write pid file: %v\n", err)
			os.Exit(1)
		}
		time.Sleep(60 * time.Second)
	case "parent_sleep_child_burn":
		// Waits without using CPU while a child burns CPU for arg(0) milliseconds.
		child := spawnHelper("burn_cpu", arg(0))
		if err := child.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Child failed: %v\n", err)
			os.Exit(1)
		}
	case "exit_code_42":
		// Exits with code 42 to verify non-zero exit codes are preserved.
		os.Exit(42)
	case "read_stdin":
		// Reads from standard input. Expects immediate EOF if stdin is disconnected.
		buf := make([]byte, 16)
		n, err := os.Stdin.Read(buf)
		if err == io.EOF || n == 0 {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Unexpected data read from stdin: %d bytes, err: %v\n", n, err)
		os.Exit(1)
	default:
		if f, ok := platformHelpers[cmd]; ok {
			f(cmdArgs)
			return
		}
		fmt.Fprintf(os.Stderr, "Unknown helper command: %q\n", cmd)
		os.Exit(2)
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the buffer contents.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var (
	logCaptureOnce sync.Once
	logCapture     = &syncBuffer{}
)

// capturedLogsSince returns a function that yields log output written after this call.
func capturedLogsSince() func() string {
	logCaptureOnce.Do(func() { logger.Init("supervisor_test", false, false, logCapture) })
	start := len(logCapture.String())
	return func() string { return logCapture.String()[start:] }
}

// persistentDialog returns a detector that always reports the same dialog.
func persistentDialog(hwnd uintptr) windowDetectorFunc {
	return func(pids []uint32) ([]windowInfo, error) {
		return []windowInfo{{
			PID:       pids[0],
			HWND:      hwnd,
			Title:     "License Agreement",
			ClassName: "#32770",
			ExePath:   "installer.exe",
		}}, nil
	}
}

// supportsCPUAccounting reports whether the platform measures child CPU time.
func supportsCPUAccounting() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "windows"
}

// TestRun_InactivityTimeout verifies that a stalled process is aborted after InactivityTimeout.
func TestRun_InactivityTimeout(t *testing.T) {
	cmd := helperCommand(t, "stall")
	opts := Options{
		InactivityTimeout: 300 * time.Millisecond,
		pollInterval:      20 * time.Millisecond,
	}

	start := time.Now()
	err := Run(cmd, opts, io.Discard)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrInactivityTimeout) {
		t.Fatalf("Run got error %v, want ErrInactivityTimeout", err)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("Run took %v, want at least the 300ms inactivity timeout", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Errorf("Run took %v, want termination shortly after the 300ms timeout", elapsed)
	}
}

// TestRun_StdinDisconnected verifies that reading child stdin returns EOF immediately.
func TestRun_StdinDisconnected(t *testing.T) {
	cmd := helperCommand(t, "read_stdin")
	opts := Options{InactivityTimeout: -1, pollInterval: 50 * time.Millisecond}

	if err := Run(cmd, opts, io.Discard); err != nil {
		t.Fatalf("Run returned error %v, want clean exit 0 on stdin EOF", err)
	}
}

// TestRun_StdinDisconnected_PreexistingStdin verifies that caller-provided stdin is replaced.
func TestRun_StdinDisconnected_PreexistingStdin(t *testing.T) {
	cmd := helperCommand(t, "read_stdin")
	cmd.Stdin = strings.NewReader("unexpected stdin input\n")
	opts := Options{InactivityTimeout: -1, pollInterval: 50 * time.Millisecond}

	if err := Run(cmd, opts, io.Discard); err != nil {
		t.Fatalf("Run returned error %v, want clean exit 0 on stdin EOF", err)
	}
}

// TestRun_OutputTee verifies that child output is copied to the out writer.
func TestRun_OutputTee(t *testing.T) {
	cmd := helperCommand(t, "unknown_subcommand_for_output")
	var out syncBuffer
	_ = Run(cmd, Options{InactivityTimeout: -1}, &out)
	if !strings.Contains(out.String(), "Unknown helper command") {
		t.Errorf("Run output %q does not contain the helper's stderr", out.String())
	}
}

// TestRun_ForwardProgress_CPUTicks verifies that CPU consumption above minCPUDelta keeps a
// process alive past InactivityTimeout.
func TestRun_ForwardProgress_CPUTicks(t *testing.T) {
	if !supportsCPUAccounting() {
		t.Skip("CPU accounting is only supported on Linux and Windows")
	}

	// The helper burns CPU for 2s while InactivityTimeout is 800ms.
	cmd := helperCommand(t, "burn_cpu", "2000")
	opts := Options{
		InactivityTimeout: 800 * time.Millisecond,
		pollInterval:      20 * time.Millisecond,
		minCPUDelta:       50 * time.Millisecond,
	}

	start := time.Now()
	err := Run(cmd, opts, io.Discard)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run returned error %v for CPU active process, want nil", err)
	}
	if elapsed < 1800*time.Millisecond {
		t.Errorf("Run completed in %v, expected at least 1.8s of CPU execution", elapsed)
	}
}

// TestRun_HardTimeout verifies that HardTimeout aborts even a process making progress.
func TestRun_HardTimeout(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "install.log")
	cmd := helperCommand(t, "active_logging", logFile, "1200", "50")
	opts := Options{
		InactivityTimeout: 5 * time.Second,
		HardTimeout:       700 * time.Millisecond,
		pollInterval:      20 * time.Millisecond,
		LogFiles:          []string{logFile},
	}
	start := time.Now()
	err := Run(cmd, opts, io.Discard)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrHardTimeout) {
		t.Fatalf("Run got error %v, want ErrHardTimeout", err)
	}
	if elapsed < 700*time.Millisecond || elapsed > 15*time.Second {
		t.Errorf("Run took %v, want between the 700ms hard timeout and 15s", elapsed)
	}
}

// TestRun_OffModeNoWatchdogs verifies that off mode disables every watchdog.
func TestRun_OffModeNoWatchdogs(t *testing.T) {
	cmd := helperCommand(t, "sleep_ms", "300")
	opts := Options{
		Mode:              ModeOff,
		InactivityTimeout: 50 * time.Millisecond,
		HardTimeout:       50 * time.Millisecond,
		UIGracePeriod:     20 * time.Millisecond,
		pollInterval:      10 * time.Millisecond,
		Unattended:        true,
		windowDetector:    persistentDialog(88),
	}
	if err := Run(cmd, opts, io.Discard); err != nil {
		t.Fatalf("Run returned %v in off mode, want nil", err)
	}
	if cmd.WaitDelay != defaultWaitDelay {
		t.Errorf("WaitDelay got %v, want %v in off mode", cmd.WaitDelay, defaultWaitDelay)
	}
}

// TestRun_WaitDelayPreserved verifies that a caller-provided WaitDelay is not overridden.
func TestRun_WaitDelayPreserved(t *testing.T) {
	cmd := helperCommand(t, "sleep_ms", "10")
	cmd.WaitDelay = 7 * time.Second
	if err := Run(cmd, Options{InactivityTimeout: -1}, io.Discard); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if cmd.WaitDelay != 7*time.Second {
		t.Errorf("WaitDelay got %v, want the caller's 7s", cmd.WaitDelay)
	}
}

// TestRun_OrphanChildCleanup verifies that terminating an inactive parent also kills its
// descendants.
func TestRun_OrphanChildCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Process verification via /proc is only supported on Linux")
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := helperCommand(t, "spawn_orphan_child", pidFile)
	opts := Options{
		InactivityTimeout: 500 * time.Millisecond,
		pollInterval:      20 * time.Millisecond,
	}

	if err := Run(cmd, opts, io.Discard); !errors.Is(err, ErrInactivityTimeout) {
		t.Fatalf("Run got error %v, want ErrInactivityTimeout", err)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("Failed to read child PID file: %v", err)
	}
	childPid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("Failed to parse child PID: %v", err)
	}

	if !waitForExit(childPid, 5*time.Second) {
		t.Errorf("Child process %d is still alive; expected process group termination", childPid)
	}
}

// TestRun_MultiLogFiles_PartialGrowth verifies that growth of any monitored log is progress.
func TestRun_MultiLogFiles_PartialGrowth(t *testing.T) {
	tmpDir := t.TempDir()
	staticLog := filepath.Join(tmpDir, "static.log")
	growingLog := filepath.Join(tmpDir, "growing.log")
	if err := os.WriteFile(staticLog, []byte("static initial content\n"), 0644); err != nil {
		t.Fatalf("Failed to write static log: %v", err)
	}
	if err := os.WriteFile(growingLog, []byte("growing initial content\n"), 0644); err != nil {
		t.Fatalf("Failed to write growing log: %v", err)
	}

	// The helper logs every 50ms for about 1.5s while InactivityTimeout is 1s.
	cmd := helperCommand(t, "active_logging", growingLog, "30", "50")
	opts := Options{
		InactivityTimeout: 1 * time.Second,
		pollInterval:      20 * time.Millisecond,
		minCPUDelta:       time.Hour,
		LogFiles:          []string{staticLog, growingLog},
	}

	start := time.Now()
	err := Run(cmd, opts, io.Discard)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run returned unexpected error %v, want nil", err)
	}
	if elapsed < 1200*time.Millisecond {
		t.Errorf("Run completed in %v, expected at least 1.2s of work", elapsed)
	}
}

// TestRun_ExitCodePreserved verifies that a non-zero exit code is preserved as an ExitError.
func TestRun_ExitCodePreserved(t *testing.T) {
	cmd := helperCommand(t, "exit_code_42")
	err := Run(cmd, Options{InactivityTimeout: -1, pollInterval: 50 * time.Millisecond}, io.Discard)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run returned error %v of type %T, want *exec.ExitError", err, err)
	}
	if exitErr.ExitCode() != 42 {
		t.Errorf("ExitCode got %d, want 42", exitErr.ExitCode())
	}
}

// TestRun_NonExistentBinary verifies that a missing binary fails fast during start.
func TestRun_NonExistentBinary(t *testing.T) {
	cmd := exec.Command("nonexistent_binary_xyz_12345")
	start := time.Now()
	err := Run(cmd, Options{InactivityTimeout: 5 * time.Second}, io.Discard)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run returned nil for non-existent binary, want error")
	}
	if errors.Is(err, ErrInactivityTimeout) {
		t.Fatal("Run returned ErrInactivityTimeout, want executable not found error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v, expected near-instant failure for non-existent binary", elapsed)
	}
}

// TestResolve verifies that zero-value Options are populated with defaults and clamped.
func TestResolve(t *testing.T) {
	opts := resolve(Options{})
	if opts.InactivityTimeout != defaultInactivityTimeout {
		t.Errorf("InactivityTimeout got %v, want %v", opts.InactivityTimeout, defaultInactivityTimeout)
	}
	if opts.UIGracePeriod != defaultUIGracePeriod {
		t.Errorf("UIGracePeriod got %v, want %v", opts.UIGracePeriod, defaultUIGracePeriod)
	}
	if opts.pollInterval != defaultPollInterval {
		t.Errorf("pollInterval got %v, want %v", opts.pollInterval, defaultPollInterval)
	}

	small := resolve(Options{InactivityTimeout: 20 * time.Millisecond})
	if small.pollInterval != 10*time.Millisecond {
		t.Errorf("Clamped pollInterval got %v, want 10ms minimum", small.pollInterval)
	}
	if small.progressWindow != 20*time.Millisecond {
		t.Errorf("progressWindow got %v, want it clamped to the 20ms inactivity timeout", small.progressWindow)
	}
}

// TestProgressTracker verifies rolling-window thresholds on synthetic samples.
func TestProgressTracker(t *testing.T) {
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	const window = 30 * time.Second
	const minCPU = 250 * time.Millisecond
	const minIO = 64 * 1024

	t.Run("NoiseBelowThresholdIsNotProgress", func(t *testing.T) {
		tr := newProgressTracker(window, minCPU, minIO, base)
		var cpu time.Duration
		// 10ms of CPU and 1KiB of I/O every 2s is 150ms and 15KiB per 30s, below the thresholds.
		for i := 1; i <= 60; i++ {
			cpu += 10 * time.Millisecond
			if tr.observe(progressSample{at: at(float64(2 * i)), cpu: cpu, io: uint64(i) * 1024}, nil) {
				t.Fatalf("Tick %d counted noise as progress", i)
			}
		}
		if !tr.lastProgress.Equal(base) {
			t.Errorf("lastProgress got %v, want unchanged %v", tr.lastProgress, base)
		}
	})

	t.Run("CPUAboveThresholdWithinWindow", func(t *testing.T) {
		tr := newProgressTracker(window, minCPU, minIO, base)
		tr.observe(progressSample{at: at(0)}, nil)
		if tr.observe(progressSample{at: at(2), cpu: 100 * time.Millisecond}, nil) {
			t.Fatal("100ms of CPU counted as progress")
		}
		if !tr.observe(progressSample{at: at(4), cpu: 260 * time.Millisecond}, nil) {
			t.Fatal("260ms of CPU within the window did not count as progress")
		}
	})

	t.Run("OldBurstLeavesWindow", func(t *testing.T) {
		tr := newProgressTracker(window, minCPU, minIO, base)
		tr.observe(progressSample{at: at(0)}, nil)
		tr.observe(progressSample{at: at(1), cpu: time.Second}, nil)
		if !tr.observe(progressSample{at: at(20), cpu: time.Second}, nil) {
			t.Fatal("A burst 19s ago within the 30s window was not progress")
		}
		if tr.observe(progressSample{at: at(40), cpu: time.Second}, nil) {
			t.Fatal("A burst 39s ago outside the 30s window was progress")
		}
		if !tr.lastProgress.Equal(at(20)) {
			t.Errorf("lastProgress got %v, want %v", tr.lastProgress, at(20))
		}
	})

	t.Run("IOAboveThreshold", func(t *testing.T) {
		tr := newProgressTracker(window, minCPU, minIO, base)
		tr.observe(progressSample{at: at(0)}, nil)
		if !tr.observe(progressSample{at: at(2), io: minIO}, nil) {
			t.Fatal("I/O equal to the threshold did not count as progress")
		}
	})

	t.Run("NegativeMinCPUDisablesCPUSignal", func(t *testing.T) {
		tr := newProgressTracker(window, -1, minIO, base)
		tr.observe(progressSample{at: at(0)}, nil)
		if tr.observe(progressSample{at: at(2), cpu: time.Hour}, nil) {
			t.Fatal("CPU counted as progress with a negative minCPUDelta")
		}
	})

	t.Run("LogGrowthZeroThreshold", func(t *testing.T) {
		tr := newProgressTracker(window, minCPU, minIO, base)
		tr.observe(progressSample{at: at(0)}, map[string]int64{"a": 10})
		if tr.observe(progressSample{at: at(2)}, map[string]int64{"a": 10}) {
			t.Fatal("Unchanged log counted as progress")
		}
		if !tr.observe(progressSample{at: at(4)}, map[string]int64{"a": 11}) {
			t.Fatal("One byte of log growth did not count as progress")
		}
		if !tr.observe(progressSample{at: at(6)}, map[string]int64{"a": 11, "b": 0}) {
			t.Fatal("A newly appearing log file did not count as progress")
		}
		if tr.observe(progressSample{at: at(8)}, map[string]int64{"a": 5, "b": 0}) {
			t.Fatal("A truncated log counted as progress")
		}
	})
}

// TestPIDCounters verifies monotonic aggregation across appearing and exiting processes.
func TestPIDCounters(t *testing.T) {
	p1, p2, p3 := procKey{pid: 1, created: 10}, procKey{pid: 2, created: 20}, procKey{pid: 3, created: 30}
	var p pidCounters
	if cpu, io := p.update(map[procKey]counters{p1: {cpu: time.Second, io: 100}}); cpu != 0 || io != 0 {
		t.Fatalf("Priming update got (%v, %d), want zero", cpu, io)
	}
	cpu, io := p.update(map[procKey]counters{p1: {cpu: 2 * time.Second, io: 150}, p2: {cpu: 300 * time.Millisecond, io: 10}})
	if cpu != 1300*time.Millisecond || io != 60 {
		t.Errorf("Update got (%v, %d), want (1.3s, 60)", cpu, io)
	}
	// Process 1 exits; process 2 advances.
	cpu, io = p.update(map[procKey]counters{p2: {cpu: 400 * time.Millisecond, io: 10}})
	if cpu != 100*time.Millisecond || io != 0 {
		t.Errorf("Update after exit got (%v, %d), want (100ms, 0)", cpu, io)
	}
	p.reset()
	if cpu, _ := p.update(map[procKey]counters{p3: {cpu: time.Hour}}); cpu != 0 {
		t.Errorf("Update after reset got %v, want zero", cpu)
	}
}

// TestPIDCounters_TransientDropout verifies that a process missing from one sample is credited
// only with its increase when it reappears, while a reused PID with a different creation time
// counts as a new process.
func TestPIDCounters_TransientDropout(t *testing.T) {
	svc := procKey{pid: 100, created: 1}
	var p pidCounters
	p.update(map[procKey]counters{svc: {cpu: time.Hour, io: 1 << 30}})
	if cpu, io := p.update(map[procKey]counters{}); cpu != 0 || io != 0 {
		t.Fatalf("Update with the process missing got (%v, %d), want zero", cpu, io)
	}
	cpu, io := p.update(map[procKey]counters{svc: {cpu: time.Hour + 10*time.Millisecond, io: 1<<30 + 5}})
	if cpu != 10*time.Millisecond || io != 5 {
		t.Errorf("Update after the process reappeared got (%v, %d), want (10ms, 5), not its lifetime counters", cpu, io)
	}
	reused := procKey{pid: 100, created: 2}
	if cpu, _ := p.update(map[procKey]counters{reused: {cpu: 50 * time.Millisecond}}); cpu != 50*time.Millisecond {
		t.Errorf("Update for a reused PID got %v, want its full 50ms", cpu)
	}
}

// noProgress is a progressedSince function that never reports progress.
func noProgress(time.Time) bool { return false }

// alwaysProgress is a progressedSince function that always reports progress.
func alwaysProgress(time.Time) bool { return true }

// TestUISnifferState_ProgressResets verifies that progress after a window appeared resets its
// timer.
func TestUISnifferState_ProgressResets(t *testing.T) {
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	grace := 30 * time.Second
	w := []windowInfo{{HWND: 1, Title: "A", ClassName: "#32770"}}
	var s uiSnifferState
	s.check(w, grace, base, noProgress)
	if abort, _ := s.check(w, grace, base.Add(20*time.Second), alwaysProgress); abort {
		t.Fatal("Unexpected abort on a tick with progress")
	}
	if abort, _ := s.check(w, grace, base.Add(45*time.Second), noProgress); abort {
		t.Fatal("Unexpected abort 25s after progress reset the timer")
	}
	if abort, _ := s.check(w, grace, base.Add(50*time.Second), noProgress); !abort {
		t.Fatal("Expected abort 30s after the last progress")
	}
}

// TestProgressedSince verifies that only progress strictly after the given time counts.
func TestProgressedSince(t *testing.T) {
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	tr := newProgressTracker(30*time.Second, 250*time.Millisecond, 64*1024, base)
	tr.observe(progressSample{at: at(0)}, nil)
	tr.observe(progressSample{at: at(2), cpu: time.Second}, nil)
	tr.observe(progressSample{at: at(4), cpu: time.Second}, nil)
	if !tr.progressedSince(at(0)) {
		t.Error("progressedSince(0s) got false, want true for a 1s burst at 2s")
	}
	if tr.progressedSince(at(2)) {
		t.Error("progressedSince(2s) got true, want false because the burst ended at 2s")
	}
	tr.observe(progressSample{at: at(6), cpu: 1100 * time.Millisecond}, nil)
	if tr.progressedSince(at(2)) {
		t.Error("progressedSince(2s) got true for 100ms of CPU, want false")
	}
	tr.observe(progressSample{at: at(8), cpu: 1300 * time.Millisecond}, nil)
	if !tr.progressedSince(at(2)) {
		t.Error("progressedSince(2s) got false for 300ms of CPU, want true")
	}
	if tr.progressedSince(at(6)) {
		t.Error("progressedSince(6s) got true for 200ms of CPU, want false")
	}
	if tr.progressedSince(at(8)) {
		t.Error("progressedSince(8s) got true at the latest sample, want false")
	}

	logs := newProgressTracker(30*time.Second, 250*time.Millisecond, 64*1024, base)
	logs.observe(progressSample{at: at(0)}, map[string]int64{"a": 1})
	logs.observe(progressSample{at: at(2)}, map[string]int64{"a": 2})
	logs.observe(progressSample{at: at(4)}, map[string]int64{"a": 2})
	if !logs.progressedSince(at(0)) {
		t.Error("progressedSince(0s) got false, want true for log growth at 2s")
	}
	if logs.progressedSince(at(2)) {
		t.Error("progressedSince(2s) got true, want false because the log last grew at 2s")
	}
}

// TestConfigure verifies process-wide defaults and how per-call options combine with them.
func TestConfigure(t *testing.T) {
	t.Cleanup(func() { Configure(Options{}) })

	Configure(Options{Mode: ModeMonitor, InactivityTimeout: -1, Unattended: true})
	d := CurrentDefaults()
	if d.Mode != ModeMonitor || d.InactivityTimeout != -1 || !d.Unattended || d.HardTimeout != defaultHardTimeout || d.pollInterval != defaultPollInterval {
		t.Errorf("CurrentDefaults() after Configure = %+v, want monitor, inactivity disabled, unattended and built-in values elsewhere", d)
	}
	got := resolve(Options{})
	if got.Mode != ModeMonitor || got.InactivityTimeout != -1 || !got.Unattended || got.HardTimeout != defaultHardTimeout {
		t.Errorf("resolve(Options{}) after Configure = %+v, want the configured defaults", got)
	}
	got = resolve(Options{Mode: ModeEnforce, InactivityTimeout: time.Minute, Unattended: false})
	if got.Mode != ModeEnforce || got.InactivityTimeout != time.Minute {
		t.Errorf("resolve with per-call overrides = %+v, want enforce and 1m", got)
	}
	if !got.Unattended {
		t.Error("resolve cleared Unattended; per-call options can only enable it")
	}

	Configure(Options{DisableUIDetection: true})
	if !resolve(Options{}).DisableUIDetection {
		t.Error("resolve(Options{}).DisableUIDetection got false after Configure enabled it")
	}

	Configure(Options{})
	d = CurrentDefaults()
	if d.Mode != defaultMode || d.InactivityTimeout != defaultInactivityTimeout || d.Unattended || d.DisableUIDetection {
		t.Errorf("CurrentDefaults() after Configure(Options{}) = %+v, want built-in defaults", d)
	}
	if !resolve(Options{Unattended: true}).Unattended {
		t.Error("resolve(Options{Unattended: true}).Unattended got false")
	}
}

// TestAdminHardTimeoutConfigured verifies that Configure records whether HardTimeout was set,
// including to the built-in default's value.
func TestAdminHardTimeoutConfigured(t *testing.T) {
	t.Cleanup(func() { Configure(Options{}) })
	for _, tc := range []struct {
		name string
		hard time.Duration
		want bool
	}{
		{"Unset", 0, false},
		{"ExplicitBuiltinValue", defaultHardTimeout, true},
		{"Custom", 3 * time.Hour, true},
		{"Disabled", -1, true},
	} {
		Configure(Options{HardTimeout: tc.hard})
		if got := adminHardTimeoutConfigured(); got != tc.want {
			t.Errorf("%s: adminHardTimeoutConfigured() after Configure(HardTimeout: %v) = %v, want %v", tc.name, tc.hard, got, tc.want)
		}
	}
}

// TestServicingOptions verifies the wusa and DISM servicing policy.
func TestServicingOptions(t *testing.T) {
	windir := filepath.Join("X:", "Win")
	cbs := filepath.Join(windir, "Logs", "CBS", "CBS.log")
	for _, tc := range []struct {
		name           string
		opts           Options
		adminHard      bool
		windir         string
		wantHard       time.Duration
		wantLogs       []string
		wantInactivity time.Duration
	}{
		{"NothingConfiguredLiftsCap", Options{LogFiles: []string{"pkg.msu.log"}}, false, windir, -1, []string{"pkg.msu.log", cbs}, 0},
		{"AdminExplicitHardTimeoutRespected", Options{}, true, windir, 0, []string{cbs}, 0},
		{"PackageTimeoutRespected", Options{HardTimeout: 2 * time.Hour, InactivityTimeout: 20 * time.Minute}, false, windir, 2 * time.Hour, []string{cbs}, 20 * time.Minute},
		{"PackageTimeoutWinsOverAdmin", Options{HardTimeout: 2 * time.Hour}, true, windir, 2 * time.Hour, []string{cbs}, 0},
		{"EmptyWindirFallsBack", Options{}, false, "", -1, []string{filepath.Join(`C:\Windows`, "Logs", "CBS", "CBS.log")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := servicingOptions(tc.opts, tc.adminHard, tc.windir)
			if got.HardTimeout != tc.wantHard {
				t.Errorf("servicingOptions().HardTimeout = %v, want %v", got.HardTimeout, tc.wantHard)
			}
			if got.InactivityTimeout != tc.wantInactivity {
				t.Errorf("servicingOptions().InactivityTimeout = %v, want %v", got.InactivityTimeout, tc.wantInactivity)
			}
			if !slices.Equal(got.LogFiles, tc.wantLogs) {
				t.Errorf("servicingOptions().LogFiles = %v, want %v", got.LogFiles, tc.wantLogs)
			}
		})
	}

	// The caller's LogFiles slice must not be modified through a shared backing array.
	orig := make([]string, 1, 4)
	orig[0] = "a.log"
	servicingOptions(Options{LogFiles: orig}, false, windir)
	if extended := orig[:2]; extended[1] != "" {
		t.Errorf("servicingOptions wrote %q into the caller's LogFiles backing array", extended[1])
	}
}

// TestFallbackOptions verifies that without a Job Object only the hard timeout stays enforced.
func TestFallbackOptions(t *testing.T) {
	in := testOptions(Options{InactivityTimeout: 5 * time.Minute, HardTimeout: time.Hour, Unattended: true})
	got := fallbackOptions(in)
	if got.InactivityTimeout != -1 || !got.DisableUIDetection {
		t.Errorf("fallbackOptions() = InactivityTimeout %v, DisableUIDetection %v; want -1 and true", got.InactivityTimeout, got.DisableUIDetection)
	}
	if got.HardTimeout != time.Hour || got.Mode != in.Mode {
		t.Errorf("fallbackOptions() = HardTimeout %v, Mode %v; want %v and %v unchanged", got.HardTimeout, got.Mode, time.Hour, in.Mode)
	}
	if w := newWatchdog(got, time.Now()); w.uiEnabled() {
		t.Error("UI detection is enabled after fallbackOptions")
	}

	off := testOptions(Options{Mode: ModeOff, InactivityTimeout: time.Minute})
	if got := fallbackOptions(off); got.InactivityTimeout != time.Minute || got.DisableUIDetection {
		t.Errorf("fallbackOptions(off mode) = %+v, want the options unchanged", got)
	}
}

// TestSupervise_FallbackKeepsWorkingRootAlive verifies with fallback options that a root process
// showing no progress of its own is terminated only by the hard timeout.
func TestSupervise_FallbackKeepsWorkingRootAlive(t *testing.T) {
	tree := &fakeTree{winsAt: persistentWindow(3, "Setup")}
	opts := fallbackOptions(testOptions(Options{InactivityTimeout: time.Minute, HardTimeout: 10 * time.Minute, Unattended: true}))
	err := runFake(opts, tree, 2*time.Second, 15*time.Minute)
	if !errors.Is(err, ErrHardTimeout) {
		t.Fatalf("supervise got %v, want ErrHardTimeout", err)
	}
	if tree.terminatedAt != 10*time.Minute || tree.windowCalls != 0 {
		t.Errorf("Terminated at %v with %d windows() call(s), want 10m and 0", tree.terminatedAt, tree.windowCalls)
	}
}

// TestWatchdog_MonitorDedup verifies that monitor mode reports each reason once until it clears.
func TestWatchdog_MonitorDedup(t *testing.T) {
	logs := capturedLogsSince()
	w := newWatchdog(Options{Mode: ModeMonitor}, time.Now())
	d := &abortDecision{ErrInactivityTimeout, "details"}
	for i := 0; i < 5; i++ {
		if w.shouldTerminate(d) {
			t.Fatal("shouldTerminate returned true in monitor mode")
		}
	}
	w.shouldTerminate(nil)
	w.shouldTerminate(d)
	if n := strings.Count(logs(), "WOULD_KILL: "); n != 2 {
		t.Errorf("Got %d WOULD_KILL lines, want 2 (one per episode)", n)
	}
	if !newWatchdog(Options{Mode: ModeEnforce}, time.Now()).shouldTerminate(d) {
		t.Error("shouldTerminate returned false in enforce mode")
	}
}

// TestCommandDetection verifies msiexec and servicing command detection across path styles.
func TestCommandDetection(t *testing.T) {
	for _, tc := range []struct {
		path      string
		msiexec   bool
		servicing bool
	}{
		{`C:\Windows\System32\msiexec.exe`, true, false},
		{`C:\Windows\System32\MSIEXEC.EXE`, true, false},
		{"msiexec", true, false},
		{"/usr/bin/msiexec", true, false},
		{`"C:\Windows\msiexec.exe"`, true, false},
		{`C:\Windows\msiexec2.exe`, false, false},
		{`C:\Windows\System32\wusa.exe`, false, true},
		{"WUSA", false, true},
		{`C:\Windows\System32\Dism.exe`, false, true},
		{"setup.exe", false, false},
		{"", false, false},
	} {
		if got := isMsiexecCommand(tc.path); got != tc.msiexec {
			t.Errorf("isMsiexecCommand(%q) got %v, want %v", tc.path, got, tc.msiexec)
		}
		if got := isServicingCommand(tc.path); got != tc.servicing {
			t.Errorf("isServicingCommand(%q) got %v, want %v", tc.path, got, tc.servicing)
		}
	}
}

// procTable is a fake process table with creation times for tree-walk tests.
type procTable struct {
	entries []procEntry
	created map[uint32]time.Time
}

// createdAt returns the creation time of pid, if known.
func (p procTable) createdAt(pid uint32) (time.Time, bool) {
	t, ok := p.created[pid]
	return t, ok
}

// newProcTable returns a process table modeled on an MSI transaction with a custom action host,
// a TrustedInstaller servicing operation, unrelated processes and a reused PID.
func newProcTable(base time.Time) procTable {
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	p := procTable{created: make(map[uint32]time.Time)}
	add := func(pid, ppid uint32, exe string, created int) {
		p.entries = append(p.entries, procEntry{pid: pid, ppid: ppid, exe: exe})
		if created >= 0 {
			p.created[pid] = at(created)
		}
	}
	add(100, 4, "msiexec.exe", 0)       // The msiserver service process (msiexec /V).
	add(200, 100, "MsiExec.exe", 10)    // A custom action host (msiexec -Embedding).
	add(300, 200, "helper.exe", 20)     // An EXE launched by the custom action host.
	add(301, 300, "cmd.exe", 21)        // A grandchild of the custom action host.
	add(400, 4, "explorer.exe", 5)      // An unrelated process.
	add(401, 400, "notepad.exe", 6)     // A child of the unrelated process.
	add(500, 200, "stale.exe", 5)       // Reuses a PID whose recorded parent is 200 but predates it.
	add(501, 500, "stalechild.exe", 30) // A child of the reused PID, unreachable from the service.
	add(600, 100, "unknown.exe", -1)    // A child whose creation time cannot be read.
	add(900, 4, "TrustedInstaller.exe", 0)
	add(901, 900, "TiWorker.exe", 2)
	add(700, 8, "TiWorker.exe", 1) // A TiWorker started by DcomLaunch rather than the service.
	add(701, 700, "conhost.exe", 2)
	add(800, 801, "a.exe", 1) // A parent cycle, which must not loop forever.
	add(801, 800, "b.exe", 1)
	return p
}

// TestProcessForest verifies the service tree walk.
func TestProcessForest(t *testing.T) {
	p := newProcTable(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		name  string
		roots []uint32
		want  []uint32
	}{
		{"MSIServiceTreeIncludesServiceAndAllDescendants", []uint32{100}, []uint32{100, 200, 300, 301}},
		{"NoServiceRunning", []uint32{0}, nil},
		{"UnrelatedTree", []uint32{400}, []uint32{400, 401}},
		{"TrustedInstallerWithTiWorkerRoots", append([]uint32{900}, pidsWithImage(p.entries, tiWorkerImage)...), []uint32{900, 901, 700, 701}},
		{"ParentCycle", []uint32{800}, []uint32{800, 801}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := processForest(p.entries, tc.roots, p.createdAt); !slices.Equal(got, tc.want) {
				t.Errorf("processForest(%v) got %v, want %v", tc.roots, got, tc.want)
			}
		})
	}
}

// TestSelectTerminable verifies that only descendants created after supervision started may be
// terminated, and never a protected service process.
func TestSelectTerminable(t *testing.T) {
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	p := newProcTable(base)
	tree := []uint32{100, 200, 300, 301, 600}
	for _, tc := range []struct {
		name      string
		notBefore time.Time
		want      []uint32
	}{
		{"AllDescendantsAfterStart", base, []uint32{200, 300, 301}},
		{"OnlyLaterDescendants", base.Add(15 * time.Second), []uint32{300, 301}},
		{"NoneAfterStart", base.Add(time.Hour), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectTerminable(tree, []uint32{100}, p.createdAt, tc.notBefore); !slices.Equal(got, tc.want) {
				t.Errorf("selectTerminable got %v, want %v", got, tc.want)
			}
		})
	}
	late := procTable{created: map[uint32]time.Time{100: base.Add(time.Hour)}}
	if got := selectTerminable([]uint32{100}, []uint32{100}, late.createdAt, base); got != nil {
		t.Errorf("selectTerminable returned the protected service PID: %v", got)
	}
}
