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
	"strings"
	"testing"
	"time"
)

// errFakeKilled is delivered as the Wait result when the fake tree is terminated.
var errFakeKilled = errors.New("fake process tree killed")

// fakeTree is a scripted supervisedTree driven by synthetic ticks.
//
// supervise calls it only from its own goroutine; tests read the recorded fields after
// supervise returns, which the done channel orders.
type fakeTree struct {
	start time.Time
	// cpuAt returns cumulative CPU time at the given elapsed time; nil means no CPU at all.
	cpuAt func(elapsed time.Duration) time.Duration
	// winsAt returns the candidate windows at the given elapsed time; call counts windows calls
	// starting at 1.
	winsAt func(elapsed time.Duration, call int) []windowInfo
	// exitAfterSamples makes rootExited return true once that many samples were taken; zero
	// means the root never exits.
	exitAfterSamples int
	waitErr          chan<- error

	now           time.Time
	samples       int
	windowCalls   int
	windowReports int
	terminated    int
	terminatedAt  time.Duration
	afterAborts   int
}

// sample records the tick time and returns the scripted counters.
func (f *fakeTree) sample(now time.Time) progressSample {
	f.now = now
	f.samples++
	s := progressSample{at: now}
	if f.cpuAt != nil {
		s.cpu = f.cpuAt(now.Sub(f.start))
	}
	return s
}

// windows returns the scripted windows for the current tick.
func (f *fakeTree) windows() []windowInfo {
	f.windowCalls++
	if f.winsAt == nil {
		return nil
	}
	wins := f.winsAt(f.now.Sub(f.start), f.windowCalls)
	if len(wins) > 0 {
		f.windowReports++
	}
	return wins
}

// terminate records the termination and completes the fake Wait.
func (f *fakeTree) terminate() {
	f.terminated++
	f.terminatedAt = f.now.Sub(f.start)
	select {
	case f.waitErr <- errFakeKilled:
	default:
	}
}

// afterAbort records the call.
func (f *fakeTree) afterAbort() { f.afterAborts++ }

// rootExited reports whether the scripted number of samples was reached.
func (f *fakeTree) rootExited() bool {
	return f.exitAfterSamples > 0 && f.samples >= f.exitAfterSamples
}

// testOptions fills unset fields from the built-in defaults without consulting process-wide
// defaults or Session 0, so tests are deterministic on every platform.
func testOptions(o Options) Options {
	return mergeOptions(o, builtinDefaults())
}

// runFake drives supervise over tree with one tick every step up to and including end. If
// supervise has not returned by then, the fake root process exits successfully.
func runFake(opts Options, tree *fakeTree, step, end time.Duration) error {
	if tree.start.IsZero() {
		tree.start = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	}
	waitErr := make(chan error, 1)
	tree.waitErr = waitErr
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- supervise(opts, waitErr, tree, tree.start, ticks) }()
	for at := step; step > 0 && at <= end; at += step {
		select {
		case ticks <- tree.start.Add(at):
		case err := <-done:
			return err
		}
	}
	select {
	case waitErr <- nil:
	default:
	}
	return <-done
}

// steadyCPU burns 500ms of CPU every 2s, well above the 250ms default threshold.
func steadyCPU(elapsed time.Duration) time.Duration { return elapsed / 4 }

// cpuUntil returns a CPU script that burns like steadyCPU until stop and then stays flat.
func cpuUntil(stop time.Duration) func(time.Duration) time.Duration {
	return func(elapsed time.Duration) time.Duration {
		if elapsed > stop {
			elapsed = stop
		}
		return steadyCPU(elapsed)
	}
}

// persistentWindow returns a window script that always reports one dialog.
func persistentWindow(hwnd uintptr, title string) func(time.Duration, int) []windowInfo {
	return func(time.Duration, int) []windowInfo {
		return []windowInfo{{PID: 1, HWND: hwnd, Title: title, ClassName: "#32770", ExePath: "setup.exe"}}
	}
}

// windowFrom returns a window script that reports one dialog from the given elapsed time on.
func windowFrom(from time.Duration) func(time.Duration, int) []windowInfo {
	return func(elapsed time.Duration, call int) []windowInfo {
		if elapsed < from {
			return nil
		}
		return persistentWindow(42, "Setup")(elapsed, call)
	}
}

// TestSupervise_InactivityTimeout verifies termination exactly at InactivityTimeout, followed by
// a single afterAbort.
func TestSupervise_InactivityTimeout(t *testing.T) {
	tree := &fakeTree{}
	err := runFake(testOptions(Options{InactivityTimeout: 5 * time.Minute}), tree, 2*time.Second, 10*time.Minute)
	if !errors.Is(err, ErrInactivityTimeout) {
		t.Fatalf("supervise got %v, want ErrInactivityTimeout", err)
	}
	if tree.terminatedAt != 5*time.Minute || tree.terminated != 1 || tree.afterAborts != 1 {
		t.Errorf("terminated %d time(s) at %v with %d afterAbort call(s), want once at 5m with one afterAbort", tree.terminated, tree.terminatedAt, tree.afterAborts)
	}
}

// TestSupervise_ProgressPreventsInactivity verifies that steady progress is never terminated
// when the hard cap is disabled.
func TestSupervise_ProgressPreventsInactivity(t *testing.T) {
	tree := &fakeTree{cpuAt: steadyCPU}
	opts := testOptions(Options{InactivityTimeout: time.Minute, HardTimeout: -1})
	if err := runFake(opts, tree, 2*time.Second, 3*time.Hour); err != nil {
		t.Fatalf("supervise got %v, want nil for a tree making progress", err)
	}
	if tree.terminated != 0 {
		t.Errorf("Tree terminated %d time(s), want 0", tree.terminated)
	}
}

// TestSupervise_HardTimeout verifies that the hard cap terminates a tree making progress.
func TestSupervise_HardTimeout(t *testing.T) {
	tree := &fakeTree{cpuAt: steadyCPU}
	err := runFake(testOptions(Options{}), tree, 2*time.Second, 2*time.Hour)
	if !errors.Is(err, ErrHardTimeout) {
		t.Fatalf("supervise got %v, want ErrHardTimeout", err)
	}
	if tree.terminatedAt != defaultHardTimeout {
		t.Errorf("Terminated at %v, want the default hard timeout %v", tree.terminatedAt, defaultHardTimeout)
	}
}

// TestSupervise_MonitorModeLogsOncePerReason verifies that monitor mode never terminates and
// logs one WOULD_KILL line per reason.
func TestSupervise_MonitorModeLogsOncePerReason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		winsAt  func(time.Duration, int) []windowInfo
		reasons []error
	}{
		{"InactivityThenHardTimeout", nil, []error{ErrInactivityTimeout, ErrHardTimeout}},
		{"UIThenHardTimeout", persistentWindow(7, "Prompt"), []error{ErrInteractiveUIDetected, ErrHardTimeout}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := capturedLogsSince()
			tree := &fakeTree{winsAt: tc.winsAt}
			opts := testOptions(Options{
				Mode:              ModeMonitor,
				InactivityTimeout: time.Minute,
				HardTimeout:       10 * time.Minute,
				Unattended:        true,
			})
			if err := runFake(opts, tree, 2*time.Second, 15*time.Minute); err != nil {
				t.Fatalf("supervise got %v in monitor mode, want nil", err)
			}
			if tree.terminated != 0 {
				t.Errorf("Tree terminated %d time(s) in monitor mode, want 0", tree.terminated)
			}
			out := logs()
			for _, r := range tc.reasons {
				if n := strings.Count(out, "WOULD_KILL: "+r.Error()); n != 1 {
					t.Errorf("Got %d WOULD_KILL lines for %q, want 1. Log:\n%s", n, r, out)
				}
			}
			if n := strings.Count(out, "WOULD_KILL: "); n != len(tc.reasons) {
				t.Errorf("Got %d WOULD_KILL lines, want %d. Log:\n%s", n, len(tc.reasons), out)
			}
		})
	}
}

// TestSupervise_OffMode verifies that off mode never samples the tree.
func TestSupervise_OffMode(t *testing.T) {
	tree := &fakeTree{}
	opts := testOptions(Options{Mode: ModeOff, InactivityTimeout: time.Nanosecond, HardTimeout: time.Nanosecond})
	if err := runFake(opts, tree, 0, 0); err != nil {
		t.Fatalf("supervise got %v in off mode, want nil", err)
	}
	if tree.samples != 0 || tree.terminated != 0 {
		t.Errorf("Off mode took %d sample(s) and terminated %d time(s), want 0 and 0", tree.samples, tree.terminated)
	}
}

// TestSupervise_UIAbortRequiresNoProgress verifies that a persistent dialog does not abort while
// the tree makes progress, and aborts one grace period after progress stops.
func TestSupervise_UIAbortRequiresNoProgress(t *testing.T) {
	tree := &fakeTree{cpuAt: cpuUntil(time.Minute), winsAt: persistentWindow(1, "Setup")}
	opts := testOptions(Options{InactivityTimeout: -1, Unattended: true})
	err := runFake(opts, tree, 2*time.Second, 5*time.Minute)
	if !errors.Is(err, ErrInteractiveUIDetected) {
		t.Fatalf("supervise got %v, want ErrInteractiveUIDetected", err)
	}
	if want := time.Minute + defaultUIGracePeriod; tree.terminatedAt != want {
		t.Errorf("Terminated at %v, want %v: the grace period after the last progress", tree.terminatedAt, want)
	}
}

// TestSupervise_UILatencyAfterStartupBurst verifies that a dialog appearing right after heavy
// startup work aborts one grace period after it appeared, not after the progress window plus
// the grace period.
func TestSupervise_UILatencyAfterStartupBurst(t *testing.T) {
	tree := &fakeTree{cpuAt: cpuUntil(10 * time.Second), winsAt: windowFrom(12 * time.Second)}
	opts := testOptions(Options{InactivityTimeout: -1, Unattended: true})
	err := runFake(opts, tree, 2*time.Second, 5*time.Minute)
	if !errors.Is(err, ErrInteractiveUIDetected) {
		t.Fatalf("supervise got %v, want ErrInteractiveUIDetected", err)
	}
	if want := 12*time.Second + defaultUIGracePeriod; tree.terminatedAt != want {
		t.Errorf("Terminated at %v, want %v: one grace period after the dialog appeared", tree.terminatedAt, want)
	}
}

// TestSupervise_UIDetectionInactive verifies that windows are never enumerated or acted on when
// UI detection is inactive.
func TestSupervise_UIDetectionInactive(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"Attended", Options{Unattended: false}},
		{"Disabled", Options{Unattended: true, DisableUIDetection: true}},
		{"NegativeGrace", Options{Unattended: true, UIGracePeriod: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.InactivityTimeout = -1
			tree := &fakeTree{winsAt: persistentWindow(9, "Prompt")}
			if err := runFake(testOptions(tc.opts), tree, 2*time.Second, 10*time.Minute); err != nil {
				t.Fatalf("supervise got %v, want nil", err)
			}
			if tree.windowCalls != 0 {
				t.Errorf("windows() called %d time(s), want 0", tree.windowCalls)
			}
		})
	}
}

// TestSupervise_RootExitedSuppressesDecisions verifies that no abort decision is made after the
// root process exited while Wait is still pending on held output pipes.
func TestSupervise_RootExitedSuppressesDecisions(t *testing.T) {
	tree := &fakeTree{exitAfterSamples: 3}
	opts := testOptions(Options{InactivityTimeout: 10 * time.Second, HardTimeout: time.Minute})
	if err := runFake(opts, tree, 2*time.Second, 5*time.Minute); err != nil {
		t.Fatalf("supervise got %v after the root exited, want nil", err)
	}
	if tree.terminated != 0 {
		t.Errorf("Tree terminated %d time(s) after the root exited, want 0", tree.terminated)
	}
	if tree.samples != 3 {
		t.Errorf("Took %d samples, want 3: none after the root exited", tree.samples)
	}
}
