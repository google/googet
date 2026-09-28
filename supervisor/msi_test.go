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
	"slices"
	"testing"
	"time"
)

// fakeMSIService scripts the Windows Installer service for waitForMSITransaction on a
// synthetic clock that advances only when the wait sleeps.
type fakeMSIService struct {
	now time.Time
	// mutexReleasedAt, if non-zero, is when _MSIExecute is released.
	mutexReleasedAt time.Time
	// lastProgress returns the service tree's last progress time as of now.
	lastProgress func(now time.Time) time.Time
	gateOpened   time.Time
	terminable   []uint32

	observations int
	terminated   [][]uint32
	terminatedAt []time.Time
}

// env returns hooks backed by f.
func (f *fakeMSIService) env() msiWaitEnv {
	return msiWaitEnv{
		now:   func() time.Time { return f.now },
		sleep: func(d time.Duration) { f.now = f.now.Add(d) },
		mutexHeld: func() bool {
			return f.mutexReleasedAt.IsZero() || f.now.Before(f.mutexReleasedAt)
		},
		observe: func(now time.Time) serviceObservation {
			f.observations++
			return serviceObservation{
				lastProgress: f.lastProgress(now),
				lastReason:   "scripted",
				gateOpened:   f.gateOpened,
				terminable:   f.terminable,
			}
		},
		terminate: func(pids []uint32) {
			f.terminated = append(f.terminated, pids)
			f.terminatedAt = append(f.terminatedAt, f.now)
		},
	}
}

// msiTestOptions returns the built-in post-abort settings: a 5m inactivity timeout, a 10m
// mutex wait and a 2s poll.
func msiTestOptions() Options {
	return testOptions(Options{})
}

// supervisionStart and abortAt are when the tests' supervised install started and when it was
// aborted, 30m later.
var (
	supervisionStart = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	abortAt          = supervisionStart.Add(30 * time.Minute)
)

// constantTime returns a lastProgress script that always reports t.
func constantTime(t time.Time) func(time.Time) time.Time {
	return func(time.Time) time.Time { return t }
}

// TestWaitForMSITransaction_AbortRightAfterTransactionStart verifies that an abort right after
// the transaction started, when the tracker's last progress is still the supervision start,
// does not terminate custom action hosts until the tree was observed idle for a full
// InactivityTimeout after the abort.
func TestWaitForMSITransaction_AbortRightAfterTransactionStart(t *testing.T) {
	f := &fakeMSIService{
		now:          abortAt,
		lastProgress: constantTime(supervisionStart),
		gateOpened:   abortAt.Add(-2 * time.Second),
		terminable:   []uint32{200, 300},
	}
	waitForMSITransaction(msiTestOptions(), f.env())
	if len(f.terminated) != 1 {
		t.Fatalf("Terminated %d time(s), want exactly once", len(f.terminated))
	}
	if want := abortAt.Add(defaultInactivityTimeout); !f.terminatedAt[0].Equal(want) {
		t.Errorf("Terminated at %v after the abort, want %v", f.terminatedAt[0].Sub(abortAt), defaultInactivityTimeout)
	}
	if !slices.Equal(f.terminated[0], []uint32{200, 300}) {
		t.Errorf("Terminated %v, want [200 300]", f.terminated[0])
	}
}

// TestWaitForMSITransaction_GateOpenedDuringWait verifies that idle time is measured from when
// the transaction's mutex was last seen to appear if that is later than the abort.
func TestWaitForMSITransaction_GateOpenedDuringWait(t *testing.T) {
	gate := abortAt.Add(4 * time.Minute)
	f := &fakeMSIService{
		now:          abortAt,
		lastProgress: constantTime(supervisionStart),
		gateOpened:   gate,
		terminable:   []uint32{200},
	}
	waitForMSITransaction(msiTestOptions(), f.env())
	if len(f.terminatedAt) != 1 || !f.terminatedAt[0].Equal(gate.Add(defaultInactivityTimeout)) {
		t.Errorf("Terminated at %v, want once at %v", f.terminatedAt, gate.Add(defaultInactivityTimeout))
	}
}

// TestWaitForMSITransaction_ProgressingTreeNeverTerminated verifies that service-side work that
// keeps making progress is never terminated, and that the wait gives up at msiMutexWait.
func TestWaitForMSITransaction_ProgressingTreeNeverTerminated(t *testing.T) {
	f := &fakeMSIService{
		now:          abortAt,
		lastProgress: func(now time.Time) time.Time { return now },
		terminable:   []uint32{200},
	}
	waitForMSITransaction(msiTestOptions(), f.env())
	if len(f.terminated) != 0 {
		t.Errorf("Terminated a progressing service tree %d time(s) at %v, want never", len(f.terminated), f.terminatedAt)
	}
	if want := abortAt.Add(defaultMSIMutexWait); !f.now.Equal(want) {
		t.Errorf("Wait returned %v after the abort, want at msiMutexWait %v", f.now.Sub(abortAt), defaultMSIMutexWait)
	}
}

// TestWaitForMSITransaction_TerminatesOnceWhenIdle verifies a single termination one
// InactivityTimeout after the tree stopped progressing, even though it stays idle afterwards.
func TestWaitForMSITransaction_TerminatesOnceWhenIdle(t *testing.T) {
	stopped := abortAt.Add(2 * time.Minute)
	f := &fakeMSIService{
		now: abortAt,
		lastProgress: func(now time.Time) time.Time {
			if now.After(stopped) {
				return stopped
			}
			return now
		},
		terminable: []uint32{200},
	}
	waitForMSITransaction(msiTestOptions(), f.env())
	if len(f.terminatedAt) != 1 || !f.terminatedAt[0].Equal(stopped.Add(defaultInactivityTimeout)) {
		t.Errorf("Terminated at %v, want once at %v", f.terminatedAt, stopped.Add(defaultInactivityTimeout))
	}
}

// TestWaitForMSITransaction_NothingTerminable verifies that an idle tree with no descendant
// created for this install is left alone, so the service process is never terminated.
func TestWaitForMSITransaction_NothingTerminable(t *testing.T) {
	f := &fakeMSIService{now: abortAt, lastProgress: constantTime(supervisionStart)}
	waitForMSITransaction(msiTestOptions(), f.env())
	if len(f.terminated) != 0 {
		t.Errorf("Terminated %v with nothing terminable, want no termination", f.terminated)
	}
}

// TestWaitForMSITransaction_MutexWait verifies that the wait ends as soon as _MSIExecute is
// released, and not at all when it is disabled.
func TestWaitForMSITransaction_MutexWait(t *testing.T) {
	released := abortAt.Add(3 * time.Minute)
	f := &fakeMSIService{
		now:             abortAt,
		mutexReleasedAt: released,
		lastProgress:    constantTime(supervisionStart),
		terminable:      []uint32{200},
	}
	waitForMSITransaction(msiTestOptions(), f.env())
	if !f.now.Equal(released) {
		t.Errorf("Wait returned %v after the abort, want %v when the mutex was released", f.now.Sub(abortAt), released.Sub(abortAt))
	}
	if len(f.terminated) != 0 {
		t.Errorf("Terminated %v before the tree was observed idle for InactivityTimeout, want none", f.terminated)
	}

	disabled := &fakeMSIService{now: abortAt, lastProgress: constantTime(supervisionStart), terminable: []uint32{200}}
	waitForMSITransaction(testOptions(Options{msiMutexWait: -1}), disabled.env())
	if disabled.observations != 0 || !disabled.now.Equal(abortAt) {
		t.Errorf("Disabled wait took %d observation(s) and ran until %v, want none and no waiting", disabled.observations, disabled.now.Sub(abortAt))
	}
}
