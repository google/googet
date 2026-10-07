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
	"testing"
)

// dialogTicks is how many ticks that see a dialog use up the default 30s
// DialogGrace at the fake's 10s interval: the first sighting starts it.
const dialogTicks = 4

// cpuTick sends n ticks with CPU time used before each, so that a fake
// command is never inactive yet does no I/O. Activity added before a send may
// already be seen on the previous tick, so at most one tick in a row sees no
// change.
func (f *fakeCmd) cpuTick(t *testing.T, n int) {
	t.Helper()
	for range n {
		f.cpu.Add(1)
		f.tick(t, 1)
	}
}

func TestSuperviseEnforceKillsOnDialog(t *testing.T) {
	// The dialog's message loop uses CPU time, which does not count.
	f := superviseFake(t, InactivityEnforce, 0)
	f.dialog.Store(true)
	f.cpuTick(t, dialogTicks)
	if err := f.wait(t); !errors.Is(err, ErrDialog) {
		t.Errorf("supervise() = %v, want %v", err, ErrDialog)
	}
	if !f.killed.Load() {
		t.Error("supervise() killed = false for a dialog nobody can answer, want true")
	}
}

func TestSuperviseDialogWithIO(t *testing.T) {
	for _, tt := range []struct {
		name string
		work func(f *fakeCmd)
	}{
		{"io", func(f *fakeCmd) { f.io.Add(1) }},
		{"output", func(f *fakeCmd) { f.out.Write([]byte("x")) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := superviseFake(t, InactivityEnforce, 0)
			f.dialog.Store(true)
			// As with cpuTick, at most one tick in a row sees no change.
			for range 3 * dialogTicks {
				tt.work(f)
				f.tick(t, 1)
			}
			if err := f.finish(t); err != nil {
				t.Errorf("supervise() = %v, want nil", err)
			}
			if f.killed.Load() {
				t.Error("supervise() killed = true for a dialog while doing I/O, want false")
			}
			if got := f.warnings.Load(); got != 0 {
				t.Errorf("supervise() warnings = %d, want 0", got)
			}
		})
	}
}

func TestSuperviseDialogGraceResets(t *testing.T) {
	f := superviseFake(t, InactivityEnforce, 0)
	for range 3 {
		// A change of f.dialog may already be seen on the tick before, so
		// each stretch is seen on at most dialogTicks-1 ticks, and of the two
		// ticks without a dialog the first surely sees none.
		f.dialog.Store(true)
		f.cpuTick(t, dialogTicks-2)
		f.dialog.Store(false)
		f.cpuTick(t, 2)
	}
	if err := f.finish(t); err != nil {
		t.Errorf("supervise() = %v, want nil", err)
	}
	if f.killed.Load() {
		t.Error("supervise() killed = true for dialogs shorter than the grace period, want false")
	}
}

func TestSuperviseMonitorWarnsOncePerDialog(t *testing.T) {
	f := superviseFake(t, InactivityMonitor, 0)
	f.dialog.Store(true)
	// Ticks well past the grace period show that the warning does not repeat.
	f.cpuTick(t, 3*dialogTicks)
	if got := f.warnings.Load(); got != 1 {
		t.Errorf("supervise() warnings = %d after one dialog, want 1", got)
	}
	f.dialog.Store(false)
	f.cpuTick(t, 2)
	f.dialog.Store(true)
	// The tick after the grace period makes sure the warning has been handled.
	f.cpuTick(t, dialogTicks+1)
	if got := f.warnings.Load(); got != 2 {
		t.Errorf("supervise() warnings = %d after two dialogs, want 2", got)
	}
	if err := f.finish(t); err != nil {
		t.Errorf("supervise() = %v, want nil", err)
	}
	if f.killed.Load() {
		t.Error("supervise() killed = true in monitor mode, want false")
	}
}

func TestSuperviseDialogAfterExit(t *testing.T) {
	// The command exited, leaving its dialog to a descendant.
	f := superviseFake(t, InactivityEnforce, 0)
	f.exited.Store(true)
	f.dialog.Store(true)
	f.cpuTick(t, 3*dialogTicks)
	if err := f.finish(t); err != nil {
		t.Errorf("supervise() = %v, want nil", err)
	}
	if f.killed.Load() {
		t.Error("supervise() killed = true for a command that exited, want false")
	}
	if got := f.warnings.Load() + f.infos.Load(); got != 0 {
		t.Errorf("supervise() logged %d times for a command that exited, want 0", got)
	}
}

func TestSuperviseAnswerableDialog(t *testing.T) {
	f := superviseFake(t, InactivityEnforce, 0)
	f.answerable.Store(true)
	f.dialog.Store(true)
	// The command goes well past the inactivity limit.
	f.tick(t, 2*inactiveTicks)
	if err := f.finish(t); err != nil {
		t.Errorf("supervise() = %v, want nil", err)
	}
	if f.killed.Load() {
		t.Error("supervise() killed = true for a dialog a user can answer, want false")
	}
	if got := f.warnings.Load(); got != 1 {
		t.Errorf("supervise() warnings = %d, want 1 (inactivity)", got)
	}
	if got := f.infos.Load(); got != 1 {
		t.Errorf("supervise() infos = %d, want 1 (dialog)", got)
	}
}

func TestIsDialogWindow(t *testing.T) {
	for _, tt := range []struct {
		name    string
		class   string
		owned   bool
		exStyle uint32
		want    bool
	}{
		{name: "standard dialog", class: "#32770", want: true},
		{name: "owned modal frame", class: "WixDialog", owned: true, exStyle: wsExDlgModalFrame, want: true},
		{name: "owned without modal frame", class: "Progress", owned: true},
		{name: "unowned modal frame", class: "Main", exStyle: wsExDlgModalFrame},
		{name: "hidden helper window", class: ".NET-BroadcastEventWindow"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDialogWindow(tt.class, tt.owned, tt.exStyle); got != tt.want {
				t.Errorf("isDialogWindow(%q, %v, %#x) = %v, want %v", tt.class, tt.owned, tt.exStyle, got, tt.want)
			}
		})
	}
}

func TestSuperviseUnanswerableDialogIsNotActivity(t *testing.T) {
	f := superviseFake(t, InactivityMonitor, 0)
	f.dialog.Store(true)
	// The tick after the inactivity warning makes sure it has been handled.
	f.tick(t, inactiveTicks+1)
	if got := f.warnings.Load(); got != 2 {
		t.Errorf("supervise() warnings = %d, want 2 (dialog and inactivity)", got)
	}
	if err := f.finish(t); err != nil {
		t.Errorf("supervise() = %v, want nil", err)
	}
}
