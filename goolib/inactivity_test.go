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
	"slices"
	"strings"
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
	// resumes counts the warnings that activity resumed, and resumedAfter
	// holds the idle time the last one reported. warnings counts the others.
	resumes      atomic.Int32
	resumedAfter atomic.Int64
	warnings     atomic.Int32
	// dialog makes the command show a dialog, and answerable lets someone
	// answer it.
	dialog, answerable atomic.Bool
	infos              atomic.Int32
	// probedUnattended is what supervise last told the dialog probe.
	probedUnattended atomic.Bool
}

// superviseFake starts supervising a fake command with the given inactivity
// mode and timeout.
func superviseFake(t *testing.T, mode string, timeout time.Duration) *fakeCmd {
	t.Helper()
	f := &fakeCmd{ticks: make(chan time.Time), done: make(chan error, 1), result: make(chan error, 1)}
	p := &contained{
		kill: func() error {
			f.killed.Store(true)
			f.done <- errors.New("killed")
			return nil
		},
		exited: f.exited.Load,
		activity: func() (uint64, uint64, error) {
			if f.unreadable.Load() {
				return 0, 0, errors.New("unreadable")
			}
			return f.cpu.Load(), f.io.Load(), nil
		},
		dialog: func(unattended bool) (string, bool) {
			f.probedUnattended.Store(unattended)
			return "Setup", f.dialog.Load()
		},
	}
	w := newWatch(p.activity, &f.out, time.Minute, mode)
	if w == nil {
		t.Fatalf("newWatch(mode %q) = nil, want a watch", mode)
	}
	w.ticks = f.ticks
	w.warn = func(format string, v ...any) {
		if !strings.Contains(format, "active again") {
			f.warnings.Add(1)
			return
		}
		f.resumes.Add(1)
		if d, ok := v[len(v)-1].(time.Duration); ok {
			f.resumedAfter.Store(int64(d))
		}
	}
	w.info = func(string, ...any) { f.infos.Add(1) }
	w.unattended = func() bool { return !f.answerable.Load() }
	// Non-zero starting counters make supervise's baseline sample matter.
	f.cpu.Store(42)
	f.io.Store(42)
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

// tickUntilReturn sends up to n ticks, stopping early once supervise returns,
// and returns what supervise returned.
func (f *fakeCmd) tickUntilReturn(t *testing.T, n int) error {
	t.Helper()
	for i := range n {
		select {
		case f.ticks <- time.Time{}:
		case err := <-f.result:
			return err
		case <-time.After(10 * time.Second):
			t.Fatalf("supervise() blocked after %d ticks, want it handling ticks", i)
		}
	}
	return f.wait(t)
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
	// Activity that ends a stretch too short to warn about is not logged.
	f.tick(t, inactiveTicks-1)
	f.cpu.Add(1)
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
	// Only the end of the first warned stretch is logged.
	if got := f.resumes.Load(); got != 1 {
		t.Errorf("supervise() resumed warnings = %d, want 1", got)
	}
	if got := time.Duration(f.resumedAfter.Load()); got < time.Minute {
		t.Errorf("supervise() reported activity again after %v, want at least the 1m limit", got)
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
			f.tick(t, inactiveTicks+2)
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
	for _, tt := range []struct {
		limit, wantInterval time.Duration
	}{
		{3 * time.Second, time.Second},
		{30 * time.Second, 5 * time.Second},
	} {
		w := newWatch(func() (uint64, uint64, error) { return 0, 0, nil }, &byteCounter{}, tt.limit, InactivityEnforce)
		if w == nil || w.limit != tt.limit || w.interval != tt.wantInterval {
			t.Errorf("newWatch(%v limit) = %+v, want limit %v and interval %v", tt.limit, w, tt.limit, tt.wantInterval)
		}
	}
}

func TestSuperviseRealTicker(t *testing.T) {
	origMin, origInterval := minInactivity, minInterval
	t.Cleanup(func() { minInactivity, minInterval = origMin, origInterval })
	minInactivity, minInterval = 0, 10*time.Millisecond

	done := make(chan error, 1)
	var killed atomic.Bool
	p := &contained{
		kill: func() error {
			killed.Store(true)
			done <- errors.New("killed")
			return nil
		},
		exited:   func() bool { return false },
		activity: func() (uint64, uint64, error) { return 7, 3, nil },
	}
	w := newWatch(p.activity, &byteCounter{}, 30*time.Millisecond, InactivityEnforce)
	if err := supervise("fake", done, p, time.Hour, w); !errors.Is(err, ErrInactive) {
		t.Errorf("supervise() = %v, want %v", err, ErrInactive)
	}
	if !killed.Load() {
		t.Error("supervise() killed = false, want true")
	}
}

// render returns ds as "action: message" strings.
func render(ds []decision) []string {
	var got []string
	for _, d := range ds {
		switch d.action {
		case actKill:
			got = append(got, "kill: "+d.error().Error())
		case actInfo:
			got = append(got, "info: "+fmt.Sprintf(d.format, d.args...))
		case actResume:
			got = append(got, "resume: "+fmt.Sprintf(d.format, d.args...))
		default:
			got = append(got, "warn: "+fmt.Sprintf(d.format, d.args...))
		}
	}
	return got
}

func TestWatchStateStep(t *testing.T) {
	// With a one-minute limit sampled every 10s, the sixth idle tick reaches
	// the limit.
	unreadable := errors.New("unreadable")
	for _, tt := range []struct {
		name    string
		monitor bool
		before  watchState
		obs     observation
		want    []string
		after   watchState
	}{
		{
			name:   "activity",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 3},
			obs:    observation{cpu: 43, io: 43},
			after:  watchState{lastCPU: 43, lastIO: 43},
		},
		{
			name:   "idle below the limit",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 4},
			obs:    observation{cpu: 42, io: 42},
			after:  watchState{lastCPU: 42, lastIO: 42, idle: 5},
		},
		{
			name:   "enforce at the limit",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 5},
			obs:    observation{cpu: 42, io: 42},
			want:   []string{"kill: command inactive: fake was killed after 1m0s without activity"},
			after:  watchState{lastCPU: 42, lastIO: 42, idle: 6},
		},
		{
			// An already-exited command is neither warned about nor killed.
			name:   "enforce at the limit after exit",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 5},
			obs:    observation{cpu: 42, io: 42, exited: true},
			after:  watchState{lastCPU: 42, lastIO: 42, idle: 6},
		},
		{
			name:    "monitor at the limit",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, idle: 5},
			obs:     observation{cpu: 42, io: 42},
			want:    []string{"warn: fake has had no activity for 1m0s; it would have been killed with inactivitymode enforce"},
			after:   watchState{lastCPU: 42, lastIO: 42, idle: 6, warned: true},
		},
		{
			name:    "monitor past the limit",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, idle: 6, warned: true},
			obs:     observation{cpu: 42, io: 42},
			after:   watchState{lastCPU: 42, lastIO: 42, idle: 7, warned: true},
		},
		{
			name:    "monitor at the limit after exit",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, idle: 5},
			obs:     observation{cpu: 42, io: 42, exited: true},
			after:   watchState{lastCPU: 42, lastIO: 42, idle: 6},
		},
		{
			name:    "activity after a warned stretch",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, idle: 7, warned: true},
			obs:     observation{cpu: 43, io: 43},
			want:    []string{"resume: fake is active again after 1m10s without activity"},
			after:   watchState{lastCPU: 43, lastIO: 43},
		},
		{
			name:   "first unreadable sample",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 3},
			obs:    observation{err: unreadable},
			want:   []string{"warn: Cannot read the activity of fake, so it always counts as active: unreadable"},
			after:  watchState{unreadable: true},
		},
		{
			name:   "later unreadable sample",
			before: watchState{idle: 3, unreadable: true},
			obs:    observation{err: unreadable},
			after:  watchState{unreadable: true},
		},
		{
			// A failed sample ends the stretch but is not reported as activity.
			name:    "first unreadable sample after a warned stretch",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, idle: 7, warned: true},
			obs:     observation{err: unreadable},
			want:    []string{"warn: Cannot read the activity of fake, so it always counts as active: unreadable"},
			after:   watchState{unreadable: true},
		},
		{
			name:    "later unreadable sample after a warned stretch",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, idle: 7, warned: true, unreadable: true},
			obs:     observation{err: unreadable},
			after:   watchState{unreadable: true},
		},
		{
			// The dialog's message loop uses CPU time, which does not count.
			name:   "unanswerable dialog below the grace",
			before: watchState{lastCPU: 42, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 4}},
			obs:    observation{cpu: 43, io: 42, title: "Setup", dialog: true, unattended: true},
			after:  watchState{lastCPU: 43, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 5}},
		},
		{
			name:   "enforce at the dialog grace",
			before: watchState{lastCPU: 42, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 5}},
			obs:    observation{cpu: 43, io: 42, title: "Setup", dialog: true, unattended: true},
			want:   []string{`kill: command blocked on a dialog: fake was killed after showing a dialog for 1m0s: "Setup"`},
			after:  watchState{lastCPU: 43, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 6, dialogHandled: true}},
		},
		{
			name:    "monitor at the dialog grace",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 5}},
			obs:     observation{cpu: 43, io: 42, title: "Setup", dialog: true, unattended: true},
			want:    []string{`warn: fake has shown "Setup" for 1m0s with nobody to answer it; it would have been killed with inactivitymode enforce`},
			after:   watchState{lastCPU: 43, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 6, dialogHandled: true}},
		},
		{
			name:    "monitor at the dialog grace after exit",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 5}},
			obs:     observation{cpu: 43, io: 42, title: "Setup", dialog: true, unattended: true, exited: true},
			after:   watchState{lastCPU: 43, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 6}},
		},
		{
			name:   "dialog with I/O",
			before: watchState{lastCPU: 42, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 6, dialogHandled: true}},
			obs:    observation{cpu: 42, io: 43, title: "Setup", dialog: true, unattended: true},
			after:  watchState{lastCPU: 42, lastIO: 43},
		},
		{
			// The error names the dialog.
			name:   "enforce at the dialog grace and the limit",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 5, dialogStretch: dialogStretch{dialogSeen: 5}},
			obs:    observation{cpu: 42, io: 42, title: "Setup", dialog: true, unattended: true},
			want:   []string{`kill: command blocked on a dialog: fake was killed after showing a dialog for 1m0s: "Setup"`},
			after:  watchState{lastCPU: 42, lastIO: 42, idle: 5, dialogStretch: dialogStretch{dialogSeen: 6, dialogHandled: true}},
		},
		{
			name:    "monitor at the dialog grace and the limit",
			monitor: true,
			before:  watchState{lastCPU: 42, lastIO: 42, idle: 5, dialogStretch: dialogStretch{dialogSeen: 5}},
			obs:     observation{cpu: 42, io: 42, title: "Setup", dialog: true, unattended: true},
			want: []string{
				`warn: fake has shown "Setup" for 1m0s with nobody to answer it; it would have been killed with inactivitymode enforce`,
				"warn: fake has had no activity for 1m0s; it would have been killed with inactivitymode enforce",
			},
			after: watchState{lastCPU: 42, lastIO: 42, idle: 6, warned: true, dialogStretch: dialogStretch{dialogSeen: 6, dialogHandled: true}},
		},
		{
			// The dialog came too late to reach its grace before the limit.
			name:   "late dialog at the limit",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 5},
			obs:    observation{cpu: 42, io: 42, title: "Setup", dialog: true, unattended: true},
			want:   []string{"kill: command inactive: fake was killed after 1m0s without activity"},
			after:  watchState{lastCPU: 42, lastIO: 42, idle: 6, dialogStretch: dialogStretch{dialogSeen: 1}},
		},
		{
			name:   "answerable dialog at the grace",
			before: watchState{lastCPU: 42, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 5, wasAnswerable: true}},
			obs:    observation{cpu: 43, io: 42, title: "Setup", dialog: true},
			want:   []string{`info: fake shows "Setup", which a user can answer`},
			after:  watchState{lastCPU: 43, lastIO: 42, dialogStretch: dialogStretch{dialogSeen: 6, dialogHandled: true, wasAnswerable: true}},
		},
		{
			// From the first tick that sees it, an answerable dialog prevents the
			// kill.
			name:   "answerable dialog at the limit",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 5},
			obs:    observation{cpu: 42, io: 42, title: "Setup", dialog: true},
			want:   []string{`warn: fake has had no activity for 1m0s but shows "Setup", which a user can answer`},
			after:  watchState{lastCPU: 42, lastIO: 42, idle: 6, warned: true, dialogStretch: dialogStretch{dialogSeen: 1, wasAnswerable: true}},
		},
		{
			// Closing the dialog starts a fresh inactivity window.
			name:   "answerable dialog closed",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 7, warned: true, dialogStretch: dialogStretch{dialogSeen: 8, dialogHandled: true, wasAnswerable: true}},
			obs:    observation{cpu: 42, io: 42},
			after:  watchState{lastCPU: 42, lastIO: 42, idle: 1},
		},
		{
			name:   "answerable dialog closed with activity",
			before: watchState{lastCPU: 42, lastIO: 42, idle: 7, warned: true, dialogStretch: dialogStretch{dialogSeen: 8, dialogHandled: true, wasAnswerable: true}},
			obs:    observation{cpu: 43, io: 42},
			want:   []string{"resume: fake is active again after 1m10s without activity"},
			after:  watchState{lastCPU: 43, lastIO: 42},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := &watch{limit: time.Minute, interval: 10 * time.Second, grace: time.Minute, monitor: tt.monitor}
			s := tt.before
			s.w, s.name = w, "fake"
			got := render(s.step(tt.obs))
			if !slices.Equal(got, tt.want) {
				t.Errorf("step(%+v) = %q, want %q", tt.obs, got, tt.want)
			}
			want := tt.after
			want.w, want.name = w, "fake"
			if s != want {
				t.Errorf("step(%+v) left state %+v, want %+v", tt.obs, s, want)
			}
		})
	}
}
