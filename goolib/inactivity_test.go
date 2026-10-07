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
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// inactiveTicks is how many ticks without activity make a fake command
// inactive: newWatch samples its one-minute limit every 10s.
const inactiveTicks = 6

// fakeCmd is a command supervised with fake activity counters and clock.
type fakeCmd struct {
	ticks  chan time.Time
	done   chan error
	result chan error
	out    byteCounter
	// cpu and io are the counters of CPU time and of other activity.
	cpu, io atomic.Uint64
	// unreadable makes the activity counters fail.
	unreadable atomic.Bool
	exited     atomic.Bool
	killed     atomic.Bool
	warnings   atomic.Int32
	// dialog makes the command show a dialog, and answerable lets someone
	// answer it.
	dialog, answerable atomic.Bool
	infos              atomic.Int32
}

// superviseFake starts supervising a fake command with the given inactivity
// mode and timeout.
func superviseFake(t *testing.T, mode string, timeout time.Duration) *fakeCmd {
	t.Helper()
	f := &fakeCmd{ticks: make(chan time.Time), done: make(chan error, 1), result: make(chan error, 1)}
	p := &contained{
		kill: func() {
			f.killed.Store(true)
			f.done <- errors.New("killed")
		},
		exited: f.exited.Load,
		activity: func() (uint64, uint64, error) {
			if f.unreadable.Load() {
				return 0, 0, errors.New("unreadable")
			}
			return f.cpu.Load(), f.io.Load(), nil
		},
		dialog: func() (string, bool) { return "Setup", f.dialog.Load() },
	}
	w := newWatch(p.activity, &f.out, time.Minute, mode)
	if w == nil {
		t.Fatalf("newWatch(mode %q) = nil, want a watch", mode)
	}
	w.ticks = f.ticks
	w.warn = func(string, ...any) { f.warnings.Add(1) }
	w.info = func(string, ...any) { f.infos.Add(1) }
	w.unattended = func() bool { return !f.answerable.Load() }
	go func() { f.result <- supervise("fake", f.done, p, timeout, w) }()
	return f
}

// tick sends n ticks. Each send completes only once supervise has handled the
// previous tick.
func (f *fakeCmd) tick(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		select {
		case f.ticks <- time.Time{}:
		case err := <-f.result:
			t.Fatalf("supervise() = %v after %d ticks, want it still running", err, i)
		case <-time.After(10 * time.Second):
			t.Fatalf("supervise() blocked after %d ticks, want it handling ticks", i)
		}
	}
}

// wait returns what supervise returned.
func (f *fakeCmd) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("supervise() did not return")
		return nil
	}
}

// finish makes the command exit successfully and returns what supervise
// returned.
func (f *fakeCmd) finish(t *testing.T) error {
	t.Helper()
	f.done <- nil
	return f.wait(t)
}

func TestSuperviseEnforceKillsWhenInactive(t *testing.T) {
	f := superviseFake(t, InactivityEnforce, 0)
	f.tick(t, inactiveTicks)
	if err := f.wait(t); !errors.Is(err, ErrInactive) {
		t.Errorf("supervise() = %v, want %v", err, ErrInactive)
	}
	if !f.killed.Load() {
		t.Error("supervise() killed = false for an inactive command, want true")
	}
}

func TestSuperviseActivityPreventsKill(t *testing.T) {
	for _, tt := range []struct {
		name   string
		active func(f *fakeCmd)
		// wantWarnings is 1 when the activity counter cannot be read.
		wantWarnings int32
	}{
		{"output", func(f *fakeCmd) { f.out.Write([]byte("x")) }, 0},
		{"cpu", func(f *fakeCmd) { f.cpu.Add(1) }, 0},
		{"io", func(f *fakeCmd) { f.io.Add(1) }, 0},
		{"unreadable activity", func(f *fakeCmd) { f.unreadable.Store(true) }, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := superviseFake(t, InactivityEnforce, 0)
			for range 3 {
				f.tick(t, inactiveTicks-1)
				tt.active(f)
			}
			f.tick(t, inactiveTicks-1)
			if err := f.finish(t); err != nil {
				t.Errorf("supervise() = %v, want nil", err)
			}
			if f.killed.Load() {
				t.Error("supervise() killed = true for an active command, want false")
			}
			if got := f.warnings.Load(); got != tt.wantWarnings {
				t.Errorf("supervise() warnings = %d, want %d", got, tt.wantWarnings)
			}
		})
	}
}

func TestSuperviseMonitorWarnsOncePerInactivity(t *testing.T) {
	f := superviseFake(t, InactivityMonitor, 0)
	f.tick(t, 3*inactiveTicks)
	if got := f.warnings.Load(); got != 1 {
		t.Errorf("supervise() warnings = %d after one stretch of inactivity, want 1", got)
	}
	f.io.Add(1)
	// The send of the last tick returned before supervise sampled for it, so
	// this activity races with that sample and is seen on it or on the next
	// tick. Either way the second warning comes by tick inactiveTicks+1 and has
	// been handled once inactiveTicks+2 ticks are sent.
	f.tick(t, inactiveTicks+2)
	if got := f.warnings.Load(); got != 2 {
		t.Errorf("supervise() warnings = %d after two stretches of inactivity, want 2", got)
	}
	if err := f.finish(t); err != nil {
		t.Errorf("supervise() = %v, want nil", err)
	}
	if f.killed.Load() {
		t.Error("supervise() killed = true in monitor mode, want false")
	}
}

func TestSuperviseInactiveAfterExit(t *testing.T) {
	// The command exited but is draining output held open by a descendant.
	for _, mode := range []string{InactivityEnforce, InactivityMonitor} {
		t.Run(mode, func(t *testing.T) {
			f := superviseFake(t, mode, 0)
			f.exited.Store(true)
			f.tick(t, inactiveTicks)
			if err := f.finish(t); err != nil {
				t.Errorf("supervise() = %v, want nil", err)
			}
			if f.killed.Load() {
				t.Error("supervise() killed = true for a command that exited, want false")
			}
			if got := f.warnings.Load(); got != 0 {
				t.Errorf("supervise() warnings = %d for a command that exited, want 0", got)
			}
		})
	}
}

func TestSuperviseTimeoutInMonitorMode(t *testing.T) {
	// The hard timeout applies whatever the inactivity mode.
	f := superviseFake(t, InactivityMonitor, 50*time.Millisecond)
	if err := f.wait(t); !errors.Is(err, ErrTimeout) {
		t.Errorf("supervise() = %v, want %v", err, ErrTimeout)
	}
	if !f.killed.Load() {
		t.Error("supervise() killed = false after the timeout, want true")
	}
}

func TestNewWatch(t *testing.T) {
	activity := func() (uint64, uint64, error) { return 0, 0, nil }
	for _, tt := range []struct {
		name         string
		activity     func() (uint64, uint64, error)
		limit        time.Duration
		mode         string
		wantNil      bool
		wantLimit    time.Duration
		wantInterval time.Duration
		wantMonitor  bool
	}{
		{name: "enforce", activity: activity, limit: 5 * time.Minute, mode: InactivityEnforce, wantLimit: 5 * time.Minute, wantInterval: 10 * time.Second},
		{name: "monitor", activity: activity, limit: 5 * time.Minute, mode: InactivityMonitor, wantLimit: 5 * time.Minute, wantInterval: 10 * time.Second, wantMonitor: true},
		{name: "unknown mode", activity: activity, limit: 5 * time.Minute, mode: "bogus", wantLimit: 5 * time.Minute, wantInterval: 10 * time.Second, wantMonitor: true},
		{name: "empty mode", activity: activity, limit: 5 * time.Minute, mode: "", wantLimit: 5 * time.Minute, wantInterval: 10 * time.Second, wantMonitor: true},
		{name: "mixed case enforce", activity: activity, limit: 5 * time.Minute, mode: "Enforce", wantLimit: 5 * time.Minute, wantInterval: 10 * time.Second, wantMonitor: true},
		{name: "short limit", activity: activity, limit: 10 * time.Second, mode: InactivityEnforce, wantLimit: time.Minute, wantInterval: 10 * time.Second},
		{name: "off", activity: activity, limit: 5 * time.Minute, mode: InactivityOff, wantNil: true},
		{name: "zero limit", activity: activity, limit: 0, mode: InactivityEnforce, wantNil: true},
		{name: "negative limit", activity: activity, limit: -time.Second, mode: InactivityEnforce, wantNil: true},
		{name: "no activity counter", limit: 5 * time.Minute, mode: InactivityEnforce, wantNil: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWatch(tt.activity, &byteCounter{}, tt.limit, tt.mode)
			if tt.wantNil {
				if w != nil {
					t.Errorf("newWatch() = %+v, want nil", w)
				}
				return
			}
			if w == nil {
				t.Fatal("newWatch() = nil, want a watch")
			}
			if w.limit != tt.wantLimit || w.interval != tt.wantInterval || w.monitor != tt.wantMonitor {
				t.Errorf("newWatch() limit, interval, monitor = %v, %v, %v, want %v, %v, %v",
					w.limit, w.interval, w.monitor, tt.wantLimit, tt.wantInterval, tt.wantMonitor)
			}
		})
	}
}

func TestNewWatchIntervalFloor(t *testing.T) {
	orig := minInactivity
	t.Cleanup(func() { minInactivity = orig })
	minInactivity = 0
	w := newWatch(func() (uint64, uint64, error) { return 0, 0, nil }, &byteCounter{}, 3*time.Second, InactivityEnforce)
	if w == nil || w.limit != 3*time.Second || w.interval != time.Second {
		t.Errorf("newWatch(3s limit) = %+v, want limit 3s and interval 1s", w)
	}
}

func TestAddActivity(t *testing.T) {
	errA, errB := errors.New("a failed"), errors.New("b failed")
	counters := func(cpu, io uint64, err error) func() (uint64, uint64, error) {
		return func() (uint64, uint64, error) { return cpu, io, err }
	}
	for _, tt := range []struct {
		name            string
		a, b            func() (uint64, uint64, error)
		wantCPU, wantIO uint64
		wantErrs        []error
	}{
		{name: "sum", a: counters(1, 2, nil), b: counters(10, 20, nil), wantCPU: 11, wantIO: 22},
		{name: "a fails", a: counters(0, 0, errA), b: counters(10, 20, nil), wantCPU: 10, wantIO: 20, wantErrs: []error{errA}},
		{name: "b fails", a: counters(1, 2, nil), b: counters(0, 0, errB), wantCPU: 1, wantIO: 2, wantErrs: []error{errB}},
		{name: "both fail", a: counters(0, 0, errA), b: counters(0, 0, errB), wantErrs: []error{errA, errB}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cpu, io, err := addActivity(tt.a, tt.b)()
			if cpu != tt.wantCPU || io != tt.wantIO {
				t.Errorf("addActivity()() = %d, %d, want %d, %d", cpu, io, tt.wantCPU, tt.wantIO)
			}
			if (err != nil) != (len(tt.wantErrs) > 0) {
				t.Errorf("addActivity()() error = %v, want errors %v", err, tt.wantErrs)
			}
			for _, want := range tt.wantErrs {
				if !errors.Is(err, want) {
					t.Errorf("addActivity()() error = %v, want it to wrap %v", err, want)
				}
			}
		})
	}
}

func TestWithDescendants(t *testing.T) {
	// 1 starts 2, which starts 3 and 4; a reused PID makes 4 the parent of 2,
	// closing a cycle. 5 and its child 6 are unrelated.
	children := map[uint32][]uint32{1: {2}, 2: {3, 4}, 4: {2}, 5: {6}}
	for _, tt := range []struct {
		name  string
		roots []uint32
		want  []uint32
	}{
		{name: "tree with a cycle", roots: []uint32{1}, want: []uint32{1, 2, 3, 4}},
		{name: "overlapping roots", roots: []uint32{2, 3}, want: []uint32{2, 3, 4}},
		{name: "two trees", roots: []uint32{3, 5}, want: []uint32{3, 5, 6}},
		{name: "no roots"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			roots := slices.Clone(tt.roots)
			got := withDescendants(tt.roots, children)
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("withDescendants(%v) = %v, want %v", tt.roots, got, tt.want)
			}
			if !slices.Equal(tt.roots, roots) {
				t.Errorf("withDescendants() changed its roots to %v, want %v", tt.roots, roots)
			}
		})
	}
}
