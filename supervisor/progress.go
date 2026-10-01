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
	"os/exec"
	"sort"
	"time"

	"github.com/google/googet/v2/progress"
	"github.com/google/logger"
)

var (
	// defaultWaitDelay is applied to exec.Cmd.WaitDelay when the caller left it unset.
	defaultWaitDelay = 30 * time.Second

	// killWaitTimeout bounds how long to wait for Cmd.Wait to return after terminating the tree.
	killWaitTimeout = 60 * time.Second
)

// counters holds cumulative CPU time and I/O bytes for one process.
type counters struct {
	cpu time.Duration
	io  uint64
}

// procKey identifies one process instance. created is the process creation time in an
// OS-specific unit, or 0 if unknown; it distinguishes a reused PID from the original process.
type procKey struct {
	pid     uint32
	created int64
}

// pidCounters converts per-process cumulative counters into monotonic aggregate increments.
//
// Processes that exit simply stop contributing, so the aggregate never decreases. Processes seen
// on the first update after construction or reset form the baseline and contribute nothing;
// processes that appear later contribute their full counters because they started after the
// baseline was taken.
//
// The last-seen counters of a process that is missing from a sample are kept, so a process
// that transiently drops out, for example because it could not be opened for one poll, is
// credited only with its increase when it reappears rather than with its lifetime counters.
// A reused PID has a different creation time and is therefore a new process.
type pidCounters struct {
	primed bool
	last   map[procKey]counters
}

// update records the current per-process counters and returns the aggregate increase since the
// previous update.
func (p *pidCounters) update(cur map[procKey]counters) (time.Duration, uint64) {
	var dCPU time.Duration
	var dIO uint64
	if p.last == nil {
		p.last = make(map[procKey]counters, len(cur))
	}
	for k, c := range cur {
		if p.primed {
			prev := p.last[k]
			if c.cpu > prev.cpu {
				dCPU += c.cpu - prev.cpu
			}
			if c.io > prev.io {
				dIO += c.io - prev.io
			}
		}
		p.last[k] = c
	}
	p.primed = true
	return dCPU, dIO
}

// reset discards the baseline so that the next update primes again.
func (p *pidCounters) reset() {
	p.primed = false
	p.last = nil
}

// progressSample is a single observation of cumulative, monotonic resource counters.
type progressSample struct {
	at  time.Time
	cpu time.Duration
	io  uint64
}

// progressTracker decides whether a process tree is making meaningful forward progress.
//
// CPU and I/O are compared against thresholds over a rolling window so that timer or animation
// noise within a single poll does not count. Log file growth is a zero-threshold signal: any
// growth between two consecutive samples, or a monitored file appearing, counts as progress.
type progressTracker struct {
	window       time.Duration
	minCPU       time.Duration
	minIO        uint64
	samples      []progressSample
	logSizes     map[string]int64
	logGrewAt    time.Time
	lastProgress time.Time
	lastReason   string
}

// newProgressTracker returns a tracker whose last progress time is start.
func newProgressTracker(window, minCPU time.Duration, minIO uint64, start time.Time) *progressTracker {
	return &progressTracker{
		window:       window,
		minCPU:       minCPU,
		minIO:        minIO,
		lastProgress: start,
	}
}

// exceeds reports whether the counter increase from base to last meets a threshold, with a
// description of the signal that did.
func (t *progressTracker) exceeds(base, last progressSample) (bool, string) {
	span := last.at.Sub(base.at)
	if last.cpu > base.cpu {
		if d := last.cpu - base.cpu; t.minCPU >= 0 && d >= t.minCPU {
			return true, fmt.Sprintf("CPU +%v within %v", d, span)
		}
	}
	if last.io > base.io {
		if d := last.io - base.io; d >= t.minIO {
			return true, fmt.Sprintf("I/O +%d bytes within %v", d, span)
		}
	}
	return false, ""
}

// observe records a sample and the current log file sizes, and reports whether it constitutes
// forward progress. A nil logSizes map means log files were not sampled.
func (t *progressTracker) observe(s progressSample, logSizes map[string]int64) bool {
	progressed := false
	reason := ""

	if logSizes != nil {
		if t.logSizes != nil {
			for path, size := range logSizes {
				prev, ok := t.logSizes[path]
				if !ok || size > prev {
					progressed = true
					reason = fmt.Sprintf("log %s grew to %d bytes", path, size)
					t.logGrewAt = s.at
					break
				}
			}
		}
		t.logSizes = logSizes
	}

	t.samples = append(t.samples, s)
	// Keep exactly one sample at or before the window start so the baseline spans the full window.
	cutoff := s.at.Add(-t.window)
	drop := 0
	for drop+1 < len(t.samples) && !t.samples[drop+1].at.After(cutoff) {
		drop++
	}
	if drop > 0 {
		t.samples = append(t.samples[:0], t.samples[drop:]...)
	}

	if !progressed {
		progressed, reason = t.exceeds(t.samples[0], s)
	}
	if progressed {
		t.lastProgress = s.at
		t.lastReason = reason
	}
	return progressed
}

// progressedSince reports whether forward progress was observed strictly after since: log growth
// at a later sample, or CPU or I/O deltas at or above the thresholds measured from the first
// retained sample taken at or after since. The rolling window still bounds the baseline, so a
// burst that ended before since never counts.
func (t *progressTracker) progressedSince(since time.Time) bool {
	if len(t.samples) == 0 {
		return false
	}
	last := t.samples[len(t.samples)-1]
	if !last.at.After(since) {
		return false
	}
	if t.logGrewAt.After(since) {
		return true
	}
	base := t.samples[0]
	for _, s := range t.samples {
		if !s.at.Before(since) {
			base = s
			break
		}
	}
	ok, _ := t.exceeds(base, last)
	return ok
}

// windowDeltas returns the CPU and I/O increase across the currently retained window.
func (t *progressTracker) windowDeltas() (time.Duration, uint64, time.Duration) {
	if len(t.samples) == 0 {
		return 0, 0, 0
	}
	first, last := t.samples[0], t.samples[len(t.samples)-1]
	var dCPU time.Duration
	var dIO uint64
	if last.cpu > first.cpu {
		dCPU = last.cpu - first.cpu
	}
	if last.io > first.io {
		dIO = last.io - first.io
	}
	return dCPU, dIO, last.at.Sub(first.at)
}

// sampleLogSizes returns the current size of every existing file in paths.
func sampleLogSizes(paths []string) map[string]int64 {
	sizes := make(map[string]int64, len(paths))
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			sizes[p] = fi.Size()
		}
	}
	return sizes
}

// trackedWindow records when a candidate window was first observed.
type trackedWindow struct {
	firstSeen time.Time
	info      windowInfo
}

// uiSnifferState tracks candidate interactive windows across polling ticks.
//
// Windows are tracked per HWND so that Z-order changes do not reset their timers. A window
// disappearing forgets it. Forward progress observed after a window was first seen resets that
// window's timer, so an abort requires the window to persist for the grace period with no
// progress since it appeared. Progress that happened before the window appeared does not delay
// the abort.
type uiSnifferState struct {
	tracked map[uintptr]trackedWindow
}

// check updates the tracked windows and reports whether one has persisted for at least grace
// without intervening progress, along with diagnostics describing it. progressedSince reports
// whether progress was observed after the given time.
func (s *uiSnifferState) check(wins []windowInfo, grace time.Duration, now time.Time, progressedSince func(time.Time) bool) (bool, string) {
	if len(wins) == 0 {
		s.tracked = nil
		return false, ""
	}
	next := make(map[uintptr]trackedWindow, len(wins))
	for _, w := range wins {
		tw, ok := s.tracked[w.HWND]
		if !ok || progressedSince(tw.firstSeen) {
			tw.firstSeen = now
		}
		tw.info = w
		next[w.HWND] = tw
	}
	s.tracked = next

	// Report the longest-persisting window, breaking ties by HWND for deterministic output.
	var best *trackedWindow
	for _, h := range sortedHWNDs(next) {
		tw := next[h]
		if best == nil || tw.firstSeen.Before(best.firstSeen) {
			best = &tw
		}
	}
	if best != nil && now.Sub(best.firstSeen) >= grace {
		w := best.info
		return true, fmt.Sprintf("window persisted %v with no forward progress: PID=%d HWND=0x%x Title=%q Class=%q Image=%q",
			now.Sub(best.firstSeen).Round(time.Millisecond), w.PID, w.HWND, w.Title, w.ClassName, w.ExePath)
	}
	return false, ""
}

// sortedHWNDs returns the keys of m in ascending order.
func sortedHWNDs(m map[uintptr]trackedWindow) []uintptr {
	keys := make([]uintptr, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// watchdog combines the progress tracker, the UI sniffer and the hard timeout into abort
// decisions, and implements monitor-mode logging.
type watchdog struct {
	opts      Options
	start     time.Time
	tracker   *progressTracker
	sniffer   uiSnifferState
	wouldKill map[error]bool
}

// newWatchdog returns a watchdog for resolved options starting at start.
func newWatchdog(opts Options, start time.Time) *watchdog {
	return &watchdog{
		opts:      opts,
		start:     start,
		tracker:   newProgressTracker(opts.progressWindow, opts.minCPUDelta, opts.minIODelta, start),
		wouldKill: make(map[error]bool),
	}
}

// uiEnabled reports whether interactive window detection is active.
func (w *watchdog) uiEnabled() bool {
	return w.opts.Unattended && !w.opts.DisableUIDetection && w.opts.UIGracePeriod >= 0
}

// abortDecision describes why the watchdog wants to terminate the process tree.
type abortDecision struct {
	reason  error
	details string
}

// observe records a sample and returns an abort decision, or nil if the tree should keep
// running. The wins argument is ignored unless UI detection is enabled.
func (w *watchdog) observe(s progressSample, logSizes map[string]int64, wins []windowInfo) *abortDecision {
	now := s.at
	w.tracker.observe(s, logSizes)
	idle := now.Sub(w.tracker.lastProgress)

	if w.opts.HardTimeout > 0 && now.Sub(w.start) >= w.opts.HardTimeout {
		return &abortDecision{ErrHardTimeout, fmt.Sprintf("runtime %v exceeded hard timeout %v; %s",
			now.Sub(w.start).Round(time.Millisecond), w.opts.HardTimeout, w.diagnostics(now))}
	}
	if w.uiEnabled() {
		if abort, details := w.sniffer.check(wins, w.opts.UIGracePeriod, now, w.tracker.progressedSince); abort {
			return &abortDecision{ErrInteractiveUIDetected, details + "; " + w.diagnostics(now)}
		}
	}
	if w.opts.InactivityTimeout > 0 && idle >= w.opts.InactivityTimeout {
		return &abortDecision{ErrInactivityTimeout, fmt.Sprintf("no forward progress for %v (timeout %v); %s",
			idle.Round(time.Millisecond), w.opts.InactivityTimeout, w.diagnostics(now))}
	}
	return nil
}

// diagnostics summarizes recent progress for log messages.
func (w *watchdog) diagnostics(now time.Time) string {
	dCPU, dIO, span := w.tracker.windowDeltas()
	last := "none"
	if w.tracker.lastReason != "" {
		last = fmt.Sprintf("%s (%v ago)", w.tracker.lastReason, now.Sub(w.tracker.lastProgress).Round(time.Millisecond))
	}
	return fmt.Sprintf("runtime=%v window=%v cpu=+%v (min %v) io=+%dB (min %dB) lastProgress=%s",
		now.Sub(w.start).Round(time.Millisecond), span.Round(time.Millisecond), dCPU, w.opts.minCPUDelta, dIO, w.opts.minIODelta, last)
}

// shouldTerminate applies the mode to an abort decision. In monitor mode it logs a single
// WOULD_KILL line per reason and returns false. A reason that clears may be logged again later.
func (w *watchdog) shouldTerminate(d *abortDecision) bool {
	if d == nil {
		for r := range w.wouldKill {
			delete(w.wouldKill, r)
		}
		return false
	}
	if w.opts.Mode == ModeMonitor {
		if !w.wouldKill[d.reason] {
			w.wouldKill[d.reason] = true
			logger.Warningf("WOULD_KILL: %v %s", d.reason, d.details)
		}
		return false
	}
	// logger.Errorf always writes to stderr, so keep an active spinner from
	// overwriting the most important line of a hung install.
	progress.Interrupt(func() { logger.Errorf("Terminating installer process tree: %v %s", d.reason, d.details) })
	return true
}

// supervisedTree is the platform-specific view of a supervised process tree.
type supervisedTree interface {
	// sample returns cumulative, monotonic counters for the tree at now.
	sample(now time.Time) progressSample
	// windows returns candidate interactive windows owned by the tree.
	windows() []windowInfo
	// terminate kills every process in the tree.
	terminate()
	// afterAbort runs after the tree was terminated and reaped, for example to wait for
	// out-of-tree service work to settle.
	afterAbort()
	// rootExited reports whether the root process has exited, even if Wait has not returned yet
	// because a descendant still holds the output pipes.
	rootExited() bool
}

// supervise runs the watchdog loop over tree until waitErr delivers the result of Cmd.Wait or
// the watchdog terminates the tree. Each value received from ticks is one poll at that time.
//
// Once the root process has exited no further abort decisions are made: Wait is then only
// waiting for descendants that inherited the output pipes, which exec.Cmd.WaitDelay bounds.
func supervise(opts Options, waitErr <-chan error, tree supervisedTree, start time.Time, ticks <-chan time.Time) error {
	if opts.Mode == ModeOff {
		return normalizeExitError(<-waitErr)
	}
	wd := newWatchdog(opts, start)
	// Prime the baseline so pre-existing counters do not count as progress.
	wd.observe(tree.sample(start), sampleLogSizes(opts.LogFiles), nil)
	rootDone := false
	for {
		select {
		case err := <-waitErr:
			return normalizeExitError(err)
		case now := <-ticks:
			if rootDone {
				continue
			}
			if tree.rootExited() {
				rootDone = true
				logger.Infof("Installer process exited; waiting for its output pipes to close without further watchdog decisions.")
				continue
			}
			s := tree.sample(now)
			var wins []windowInfo
			if wd.uiEnabled() {
				wins = tree.windows()
			}
			d := wd.observe(s, sampleLogSizes(opts.LogFiles), wins)
			if wd.shouldTerminate(d) {
				tree.terminate()
				err := waitAfterTerminate(waitErr, d)
				tree.afterAbort()
				return err
			}
		}
	}
}

// setupStdio disconnects stdin and tees stdout and stderr to out when out is non-nil. It returns
// the opened null device, which the caller must close after the process exits.
func setupStdio(c *exec.Cmd, out io.Writer) (*os.File, error) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("failed to open devNull for stdin disconnection: %w", err)
	}
	c.Stdin = devNull

	if out != nil {
		c.Stdout = io.MultiWriter(progress.Stdout(), out)
		c.Stderr = io.MultiWriter(progress.Stderr(), out)
	} else {
		if c.Stdout == nil {
			c.Stdout = progress.Stdout()
		}
		if c.Stderr == nil {
			c.Stderr = progress.Stderr()
		}
	}
	if c.WaitDelay == 0 {
		c.WaitDelay = defaultWaitDelay
	}
	return devNull, nil
}

// normalizeExitError treats exec.ErrWaitDelay after a successful exit as success. The process
// itself exited cleanly; only a detached descendant still held the output pipes.
func normalizeExitError(err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		logger.Warningf("Installer exited successfully but its output pipes were still held by another process; stopped waiting after WaitDelay.")
		return nil
	}
	return err
}

// waitAfterTerminate waits for the process to be reaped after the tree was terminated and
// returns reason wrapped with details. It never blocks longer than killWaitTimeout.
func waitAfterTerminate(waitErr <-chan error, d *abortDecision) error {
	reason, details := d.reason, d.details
	select {
	case <-waitErr:
		return fmt.Errorf("%w: %s", reason, details)
	case <-time.After(killWaitTimeout):
		progress.Interrupt(func() {
			logger.Errorf("Installer process tree terminated but Wait did not return within %v; its stdout/stderr pipes are likely held by a process outside the supervised tree.", killWaitTimeout)
		})
		return fmt.Errorf("%w: %s; wait abandoned after %v because stdout/stderr pipes are held by processes outside the supervised tree", reason, details, killWaitTimeout)
	}
}
