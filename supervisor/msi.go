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
	"time"

	"github.com/google/logger"
)

const (
	// msiMutexName names the mutex the Windows Installer service holds during a transaction.
	msiMutexName = `Global\_MSIExecute`
	// serviceProgressLogInterval is how often the post-abort wait logs while waiting.
	serviceProgressLogInterval = 30 * time.Second
	// minAfterAbortPoll bounds how often the post-abort wait polls.
	minAfterAbortPoll = 10 * time.Millisecond
)

// serviceObservation is one observation of the Windows Installer service tree.
type serviceObservation struct {
	// lastProgress is when the service tree last made forward progress.
	lastProgress time.Time
	// lastReason describes the signal that last counted as progress.
	lastReason string
	// gateOpened is when the _MSIExecute mutex was last observed to appear, or the zero time.
	gateOpened time.Time
	// terminable lists service-tree descendants created for this install.
	terminable []uint32
}

// msiWaitEnv provides the clock, the _MSIExecute mutex check, service-tree sampling and
// process termination to waitForMSITransaction, so that its decisions can be tested on every
// platform.
type msiWaitEnv struct {
	now       func() time.Time
	sleep     func(time.Duration)
	mutexHeld func() bool
	observe   func(now time.Time) serviceObservation
	terminate func(pids []uint32)
}

// latest returns the latest of ts.
func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// waitForMSITransaction runs after a job that ran msiexec was terminated. It waits up to
// opts.msiMutexWait for the service-side transaction to finish so the next package does not
// collide with it.
//
// The service process itself is never terminated. Descendants created for this install are
// terminated once, and only after the whole service tree was observed idle for a full
// InactivityTimeout after the wait started and after the transaction's mutex appeared. Idle
// time is measured from the latest of the last progress, the start of the wait and the time the
// mutex was last seen to appear, so progress that could not be observed before the abort, for
// example while the mutex was absent or before the first gated sample primed the baseline,
// never makes the tree look idle. Service-side work that is making progress is never
// terminated.
func waitForMSITransaction(opts Options, env msiWaitEnv) {
	if opts.msiMutexWait < 0 {
		return
	}
	start := env.now()
	deadline := start.Add(opts.msiMutexWait)
	interval := opts.pollInterval
	if interval < minAfterAbortPoll {
		interval = minAfterAbortPoll
	}
	terminated := false
	var lastLog time.Time
	for {
		now := env.now()
		if !env.mutexHeld() {
			logger.Infof("Windows Installer transaction finished (%s released).", msiMutexName)
			return
		}
		obs := env.observe(now)
		idle := now.Sub(latest(obs.lastProgress, start, obs.gateOpened))
		if !terminated && opts.InactivityTimeout > 0 && idle >= opts.InactivityTimeout && len(obs.terminable) > 0 {
			terminated = true
			logger.Errorf("Windows Installer service tree observed inactive for %v; terminating processes %v created for this install. The service process is not terminated.", idle.Round(time.Second), obs.terminable)
			env.terminate(obs.terminable)
		}
		if !now.Before(deadline) {
			logger.Warningf("Windows Installer transaction still running after %v; continuing. Service-side processes: %v.", opts.msiMutexWait, obs.terminable)
			return
		}
		if now.Sub(lastLog) >= serviceProgressLogInterval {
			lastLog = now
			logger.Infof("Waiting for Windows Installer transaction to finish: observed idle for %v (last progress: %s).",
				idle.Round(time.Second), obs.lastReason)
		}
		env.sleep(interval)
	}
}
