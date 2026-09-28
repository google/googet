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
	"strings"
	"testing"
	"time"
)

// uiTick is the synthetic poll interval used by the UI tests.
const uiTick = 2 * time.Second

// dialog returns a candidate #32770 window.
func dialog(hwnd uintptr, title string) windowInfo {
	return windowInfo{PID: 100, HWND: hwnd, Title: title, ClassName: "#32770", ExePath: "setup.exe"}
}

// uiOptions returns unattended options with only UI detection enabled at the default grace
// period.
func uiOptions() Options {
	return testOptions(Options{InactivityTimeout: -1, HardTimeout: -1, Unattended: true})
}

// TestUI_TransientDialogsSuccessive verifies that successive distinct dialogs, each shorter than
// the grace period, do not abort even though their total duration exceeds it.
func TestUI_TransientDialogsSuccessive(t *testing.T) {
	tree := &fakeTree{winsAt: func(elapsed time.Duration, _ int) []windowInfo {
		switch {
		case elapsed < 20*time.Second:
			return []windowInfo{dialog(101, "Transient Dialog 1")}
		case elapsed >= 22*time.Second && elapsed < 42*time.Second:
			return []windowInfo{dialog(102, "Transient Dialog 2")}
		case elapsed >= 44*time.Second && elapsed < 64*time.Second:
			return []windowInfo{dialog(103, "Transient Dialog 3")}
		}
		return nil
	}}
	if err := runFake(uiOptions(), tree, uiTick, 3*time.Minute); err != nil {
		t.Fatalf("supervise got %v for successive transient dialogs, want nil", err)
	}
	if tree.windowReports < 27 {
		t.Errorf("Detector reported windows on %d ticks, want at least 27; the dialogs were not exercised", tree.windowReports)
	}
}

// TestUI_TransientDialogFlapping verifies that a dialog that closes and reopens on every other
// poll does not abort.
func TestUI_TransientDialogFlapping(t *testing.T) {
	tree := &fakeTree{winsAt: func(_ time.Duration, call int) []windowInfo {
		if call%2 == 1 {
			return []windowInfo{dialog(999, "Flapping Window")}
		}
		return nil
	}}
	if err := runFake(uiOptions(), tree, uiTick, 3*time.Minute); err != nil {
		t.Fatalf("supervise got %v for a flapping dialog, want nil", err)
	}
	if tree.windowReports < 40 {
		t.Errorf("Detector reported windows on %d ticks, want at least 40; the dialog was not exercised", tree.windowReports)
	}
}

// TestUI_ChangingHWND verifies that a dialog replaced by another before the grace period does
// not abort.
func TestUI_ChangingHWND(t *testing.T) {
	tree := &fakeTree{winsAt: func(elapsed time.Duration, _ int) []windowInfo {
		switch {
		case elapsed < 20*time.Second:
			return []windowInfo{dialog(1001, "Step 1")}
		case elapsed < 40*time.Second:
			return []windowInfo{dialog(1002, "Step 2")}
		}
		return nil
	}}
	if err := runFake(uiOptions(), tree, uiTick, 2*time.Minute); err != nil {
		t.Fatalf("supervise got %v for successive changing dialogs, want nil", err)
	}
	if tree.windowReports < 19 {
		t.Errorf("Detector reported windows on %d ticks, want at least 19", tree.windowReports)
	}
}

// TestUI_PersistentDialogTimingAndDiagnostics verifies that a persistent dialog aborts exactly
// one grace period after it was first seen, with full diagnostics.
func TestUI_PersistentDialogTimingAndDiagnostics(t *testing.T) {
	tree := &fakeTree{winsAt: func(time.Duration, int) []windowInfo {
		return []windowInfo{{PID: 100, HWND: 7777, Title: "Setup Error: Out of Disk Space", ClassName: "#32770", ExePath: `C:\Windows\Temp\setup.exe`}}
	}}
	err := runFake(uiOptions(), tree, uiTick, 5*time.Minute)
	if !errors.Is(err, ErrInteractiveUIDetected) {
		t.Fatalf("supervise got %v, want ErrInteractiveUIDetected", err)
	}
	if want := uiTick + defaultUIGracePeriod; tree.terminatedAt != want {
		t.Errorf("Terminated at %v, want %v", tree.terminatedAt, want)
	}
	for _, want := range []string{`Title="Setup Error: Out of Disk Space"`, `Class="#32770"`, `Image="C:\\Windows\\Temp\\setup.exe"`, "HWND=0x1e61"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error %q lacks %s", err, want)
		}
	}
}

// TestUI_MultiWindowPersistentDialog verifies that a persistent dialog aborts regardless of its
// position in the enumerated window list.
func TestUI_MultiWindowPersistentDialog(t *testing.T) {
	setupWin := windowInfo{PID: 100, HWND: 1001, Title: "Main Setup Window", ClassName: "SetupClass", ExePath: "setup.exe"}
	status := windowInfo{PID: 100, HWND: 1002, Title: "Worker Status", ClassName: "ProgressClass", ExePath: "setup.exe"}
	stuck := dialog(1003, "Modal Retry Dialog")
	for _, tc := range []struct {
		name      string
		winsAt    func(time.Duration, int) []windowInfo
		wantTitle string
	}{
		{"ZOrderAlternation", func(_ time.Duration, call int) []windowInfo {
			if call%2 == 1 {
				return []windowInfo{setupWin, stuck}
			}
			return []windowInfo{stuck, setupWin}
		}, ""},
		{"PersistentSecondaryWithChurningPrimary", func(_ time.Duration, call int) []windowInfo {
			churn := windowInfo{PID: 100, HWND: uintptr(5000 + call), Title: fmt.Sprintf("Progress %d", call), ClassName: "ProgressClass"}
			return []windowInfo{churn, stuck}
		}, "Modal Retry Dialog"},
		{"ThreeWindowPermutations", func(_ time.Duration, call int) []windowInfo {
			switch call % 3 {
			case 0:
				return []windowInfo{setupWin, status, stuck}
			case 1:
				return []windowInfo{status, stuck, setupWin}
			default:
				return []windowInfo{stuck, setupWin, status}
			}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := &fakeTree{winsAt: tc.winsAt}
			err := runFake(uiOptions(), tree, uiTick, 5*time.Minute)
			if !errors.Is(err, ErrInteractiveUIDetected) {
				t.Fatalf("supervise got %v, want ErrInteractiveUIDetected", err)
			}
			if want := uiTick + defaultUIGracePeriod; tree.terminatedAt != want {
				t.Errorf("Terminated at %v, want %v", tree.terminatedAt, want)
			}
			if tc.wantTitle != "" && !strings.Contains(err.Error(), fmt.Sprintf("Title=%q", tc.wantTitle)) {
				t.Errorf("Error %q does not identify %q", err, tc.wantTitle)
			}
		})
	}
}

// TestUISnifferState_Scenarios exercises the uiSnifferState state machine with synthetic time
// sequences and validates the diagnostics.
func TestUISnifferState_Scenarios(t *testing.T) {
	baseTime := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	grace := 30 * time.Second

	t.Run("EmptyWindowListResets", func(t *testing.T) {
		var s uiSnifferState
		w := []windowInfo{{HWND: 1, Title: "A", ClassName: "#32770", PID: 10, ExePath: "a.exe"}}
		if abort, _ := s.check(w, grace, baseTime, noProgress); abort {
			t.Fatal("Unexpected abort on first tick")
		}
		if abort, _ := s.check(nil, grace, baseTime.Add(10*time.Second), noProgress); abort {
			t.Fatal("Unexpected abort on empty window list")
		}
		if len(s.tracked) != 0 {
			t.Fatalf("State not reset after empty list: %+v", s)
		}
		if abort, _ := s.check(w, grace, baseTime.Add(20*time.Second), noProgress); abort {
			t.Fatal("Unexpected abort on reappearance")
		}
		if abort, _ := s.check(w, grace, baseTime.Add(45*time.Second), noProgress); abort {
			t.Fatal("Unexpected abort: elapsed 25s < grace 30s")
		}
		abort, details := s.check(w, grace, baseTime.Add(55*time.Second), noProgress)
		if !abort {
			t.Fatal("Expected abort when persisted for 35s >= 30s")
		}
		if !strings.Contains(details, `Title="A"`) || !strings.Contains(details, `Class="#32770"`) {
			t.Errorf("Diagnostic details missing expected fields: %q", details)
		}
	})

	t.Run("HWNDSwitchResetsTimer", func(t *testing.T) {
		var s uiSnifferState
		w1 := []windowInfo{{HWND: 1, Title: "A", ClassName: "#32770", PID: 10, ExePath: "a.exe"}}
		w2 := []windowInfo{{HWND: 2, Title: "B", ClassName: "#32770", PID: 10, ExePath: "a.exe"}}
		s.check(w1, grace, baseTime, noProgress)
		if abort, _ := s.check(w1, grace, baseTime.Add(25*time.Second), noProgress); abort {
			t.Fatal("Window 1 aborted prematurely at 25s")
		}
		if abort, _ := s.check(w2, grace, baseTime.Add(26*time.Second), noProgress); abort {
			t.Fatal("Unexpected abort immediately on HWND switch")
		}
		if abort, _ := s.check(w2, grace, baseTime.Add(50*time.Second), noProgress); abort {
			t.Fatal("Window 2 aborted prematurely at 24s after switch")
		}
		abort, details := s.check(w2, grace, baseTime.Add(57*time.Second), noProgress)
		if !abort {
			t.Fatal("Expected abort for Window 2 persisting > grace")
		}
		if !strings.Contains(details, `Title="B"`) {
			t.Errorf("Diagnostic details expected Window B, got: %q", details)
		}
	})

	t.Run("ZOrderAlternationDoesNotResetTimer", func(t *testing.T) {
		var s uiSnifferState
		w1 := windowInfo{HWND: 101, Title: "Installer Progress", ClassName: "SetupClass", PID: 1000, ExePath: "setup.exe"}
		w2 := windowInfo{HWND: 102, Title: "Fatal Error Prompt", ClassName: "#32770", PID: 1000, ExePath: "setup.exe"}
		for i := 0; i < 15; i++ {
			list := []windowInfo{w1, w2}
			if i%2 == 1 {
				list = []windowInfo{w2, w1}
			}
			if abort, details := s.check(list, grace, baseTime.Add(time.Duration(i*2)*time.Second), noProgress); abort {
				t.Fatalf("Unexpected abort at tick %d: %s", i, details)
			}
		}
		if abort, _ := s.check([]windowInfo{w2, w1}, grace, baseTime.Add(30*time.Second), noProgress); !abort {
			t.Fatal("Expected abort at t=30s for alternating Z-order windows")
		}
	})

	t.Run("PersistentSecondaryWithChurningPrimary", func(t *testing.T) {
		var s uiSnifferState
		modal := windowInfo{HWND: 999, Title: "Stuck Dialog", ClassName: "#32770", PID: 500, ExePath: "setup.exe"}
		for i := 0; i < 15; i++ {
			churn := windowInfo{HWND: uintptr(2000 + i), Title: fmt.Sprintf("Step %d", i), ClassName: "ProgressClass", PID: 500, ExePath: "setup.exe"}
			list := []windowInfo{churn, modal}
			if i%2 == 1 {
				list = []windowInfo{modal, churn}
			}
			if abort, details := s.check(list, grace, baseTime.Add(time.Duration(i*2)*time.Second), noProgress); abort {
				t.Fatalf("Unexpected abort at tick %d: %s", i, details)
			}
			if len(s.tracked) > 2 {
				t.Fatalf("Tracked %d windows at tick %d, want at most the 2 visible ones", len(s.tracked), i)
			}
		}
		final := windowInfo{HWND: 3000, Title: "Final Step", ClassName: "ProgressClass", PID: 500, ExePath: "setup.exe"}
		abort, details := s.check([]windowInfo{final, modal}, grace, baseTime.Add(30*time.Second), noProgress)
		if !abort {
			t.Fatal("Expected abort at t=30s for the persistent modal dialog")
		}
		if !strings.Contains(details, `Title="Stuck Dialog"`) {
			t.Errorf("Expected details to identify Stuck Dialog, got: %s", details)
		}
	})

	t.Run("ProgressBeforeFirstSeenDoesNotReset", func(t *testing.T) {
		var s uiSnifferState
		w := []windowInfo{{HWND: 7, Title: "Prompt", ClassName: "#32770"}}
		first := baseTime.Add(10 * time.Second)
		// Progress ended at 8s, before the window appeared at 10s.
		progressedSince := func(since time.Time) bool { return since.Before(baseTime.Add(8 * time.Second)) }
		s.check(w, grace, first, progressedSince)
		if abort, _ := s.check(w, grace, first.Add(20*time.Second), progressedSince); abort {
			t.Fatal("Unexpected abort 20s after the window appeared")
		}
		if abort, _ := s.check(w, grace, first.Add(grace), progressedSince); !abort {
			t.Fatal("Expected abort one grace period after the window appeared")
		}
	})
}
