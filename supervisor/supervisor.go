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

// Package supervisor provides process tree execution, forward progress monitoring,
// and interactive UI sniffing for GooGet package installers.
package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrTerminated is wrapped by every error returned when the supervisor terminates an installer.
var ErrTerminated = errors.New("installer terminated")

var (
	// ErrInactivityTimeout is returned when an installer process tree exhibits no forward progress for InactivityTimeout.
	ErrInactivityTimeout = fmt.Errorf("%w: no forward progress detected for inactivity timeout", ErrTerminated)

	// ErrInteractiveUIDetected is returned when a modal dialog persists without forward progress in unattended mode.
	ErrInteractiveUIDetected = fmt.Errorf("%w: interactive UI dialog detected in unattended mode", ErrTerminated)

	// ErrHardTimeout is returned when an installer exceeds the absolute HardTimeout regardless of progress.
	ErrHardTimeout = fmt.Errorf("%w: exceeded hard install timeout", ErrTerminated)
)

// ParseTimeout parses a configured watchdog duration. An empty string returns 0, meaning "use the
// default"; "0" returns -1, meaning "disabled"; negative durations are rejected.
func ParseTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "":
		return 0, nil
	case "0":
		return -1, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid timeout %q: must not be negative", s)
	}
	if d == 0 {
		return -1, nil
	}
	return d, nil
}

// Mode selects whether the supervisor enforces its abort decisions.
type Mode int

const (
	// ModeUnset means the process-wide default configured via Configure is used.
	ModeUnset Mode = iota
	// ModeEnforce terminates the process tree when an abort condition is met.
	ModeEnforce
	// ModeMonitor logs WOULD_KILL diagnostics when an abort condition is met but never terminates.
	ModeMonitor
	// ModeOff disables all watchdogs; the process tree is still contained and stdin is still disconnected.
	ModeOff
)

// String returns the configuration name of the mode.
func (m Mode) String() string {
	switch m {
	case ModeEnforce:
		return "enforce"
	case ModeMonitor:
		return "monitor"
	case ModeOff:
		return "off"
	default:
		return "unset"
	}
}

// ParseMode parses a configuration value ("enforce", "monitor" or "off") into a Mode.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "enforce":
		return ModeEnforce, nil
	case "monitor":
		return ModeMonitor, nil
	case "off":
		return ModeOff, nil
	default:
		return ModeUnset, fmt.Errorf("invalid supervisor mode %q: want enforce, monitor or off", s)
	}
}

// windowInfo describes an active top-level desktop window.
type windowInfo struct {
	PID       uint32
	HWND      uintptr
	Title     string
	ClassName string
	ExePath   string
}

// windowDetectorFunc inspects desktop windows belonging to the given process IDs and returns
// only windows that are candidate interactive prompts.
type windowDetectorFunc func(pids []uint32) ([]windowInfo, error)

// Options specifies watchdog behavior for supervised execution.
//
// For every duration field, zero means "use the process-wide default set by Configure" and a
// negative value disables that watchdog.
//
// The bool fields Unattended and DisableUIDetection are combined with the process-wide defaults
// by a logical OR: a per-call Options value can enable either behavior but cannot clear a value
// enabled through Configure. Unattended is additionally forced on when the current process runs
// in Windows Session 0, where no user can answer a dialog.
type Options struct {
	// Mode selects enforce, monitor or off behavior.
	Mode Mode

	// InactivityTimeout is the maximum duration without meaningful forward progress before aborting.
	InactivityTimeout time.Duration

	// HardTimeout is the absolute maximum runtime regardless of forward progress.
	HardTimeout time.Duration

	// UIGracePeriod is how long a candidate modal dialog may persist, with no forward progress
	// since it appeared, before aborting.
	UIGracePeriod time.Duration

	// DisableUIDetection turns off interactive dialog detection. It is ORed with the process-wide
	// default.
	DisableUIDetection bool

	// LogFiles is a list of file paths whose size growth indicates forward progress.
	LogFiles []string

	// Unattended indicates the installation is non-interactive, for example -noconfirm. It is
	// ORed with the process-wide default and with Session 0 detection.
	Unattended bool

	// pollInterval is the duration between watchdog samples.
	pollInterval time.Duration

	// progressWindow is the rolling window over which progress deltas are compared against thresholds.
	progressWindow time.Duration

	// minCPUDelta is the minimum aggregate CPU time consumed within progressWindow to count as progress.
	minCPUDelta time.Duration

	// minIODelta is the minimum aggregate I/O bytes transferred within progressWindow to count as progress.
	minIODelta uint64

	// msiMutexWait bounds how long to wait for the Windows Installer _MSIExecute mutex to be
	// released after a job that ran msiexec is terminated.
	msiMutexWait time.Duration

	// windowDetector overrides Win32 window enumeration in tests.
	windowDetector windowDetectorFunc
}

// Built-in defaults used when neither the caller nor Configure provides a value.
const (
	defaultMode              = ModeEnforce
	defaultInactivityTimeout = 5 * time.Minute
	defaultHardTimeout       = 60 * time.Minute
	defaultUIGracePeriod     = 30 * time.Second
	defaultPollInterval      = 2 * time.Second
	defaultProgressWindow    = 30 * time.Second
	defaultMinCPUDelta       = 250 * time.Millisecond
	defaultMinIODelta        = 64 * 1024
	defaultMSIMutexWait      = 10 * time.Minute
)

var (
	defaultsMu sync.RWMutex
	defaults   = builtinDefaults()
	// hardTimeoutConfigured records whether the last Configure call set HardTimeout, including
	// to the built-in default's value or to a negative value that disables it.
	hardTimeoutConfigured bool
)

// builtinDefaults returns the built-in process-wide defaults.
func builtinDefaults() Options {
	return Options{
		Mode:              defaultMode,
		InactivityTimeout: defaultInactivityTimeout,
		HardTimeout:       defaultHardTimeout,
		UIGracePeriod:     defaultUIGracePeriod,
		pollInterval:      defaultPollInterval,
		progressWindow:    defaultProgressWindow,
		minCPUDelta:       defaultMinCPUDelta,
		minIODelta:        defaultMinIODelta,
		msiMutexWait:      defaultMSIMutexWait,
	}
}

// Configure sets process-wide defaults. Zero-valued fields in d keep the built-in defaults.
// Unattended and DisableUIDetection are copied as-is, so Configure(Options{}) restores every
// built-in default.
func Configure(d Options) {
	merged := mergeOptions(d, builtinDefaults())
	merged.Unattended = d.Unattended
	merged.DisableUIDetection = d.DisableUIDetection
	defaultsMu.Lock()
	defaults = merged
	hardTimeoutConfigured = d.HardTimeout != 0
	defaultsMu.Unlock()
}

// CurrentDefaults returns a copy of the process-wide defaults.
func CurrentDefaults() Options {
	defaultsMu.RLock()
	defer defaultsMu.RUnlock()
	return defaults
}

// adminHardTimeoutConfigured reports whether the process-wide hard timeout was set explicitly
// through Configure rather than left at the built-in default.
func adminHardTimeoutConfigured() bool {
	defaultsMu.RLock()
	defer defaultsMu.RUnlock()
	return hardTimeoutConfigured
}

// servicingOptions adjusts per-call options for a wusa or DISM servicing operation, whose work
// runs in TrustedInstaller and TiWorker outside the installer's process tree and routinely
// takes longer than the built-in hard timeout.
//
// The built-in hard timeout is lifted unless the caller or the administrator set one; an
// explicitly configured value, even one equal to the built-in default, is respected. Growth of
// the CBS servicing log under windir, or C:\Windows if windir is empty, counts as forward
// progress so that the inactivity watchdog stays enabled.
func servicingOptions(opts Options, adminHardConfigured bool, windir string) Options {
	if opts.HardTimeout == 0 && !adminHardConfigured {
		opts.HardTimeout = -1
	}
	if windir == "" {
		windir = `C:\Windows`
	}
	logs := make([]string, 0, len(opts.LogFiles)+1)
	logs = append(logs, opts.LogFiles...)
	opts.LogFiles = append(logs, filepath.Join(windir, "Logs", "CBS", "CBS.log"))
	return opts
}

// fallbackOptions restricts resolved options to what can be enforced when no Job Object could
// be set up. Without a job, descendants can be neither measured nor terminated, so the
// inactivity and UI watchdogs would judge and kill only the root process while its children
// keep working. Only the hard timeout on the root process stays enforced.
func fallbackOptions(opts Options) Options {
	if opts.Mode == ModeOff {
		return opts
	}
	opts.InactivityTimeout = -1
	opts.DisableUIDetection = true
	return opts
}

// mergeOptions fills zero-valued fields of o from d.
func mergeOptions(o, d Options) Options {
	if o.Mode == ModeUnset {
		o.Mode = d.Mode
	}
	if o.InactivityTimeout == 0 {
		o.InactivityTimeout = d.InactivityTimeout
	}
	if o.HardTimeout == 0 {
		o.HardTimeout = d.HardTimeout
	}
	if o.UIGracePeriod == 0 {
		o.UIGracePeriod = d.UIGracePeriod
	}
	if o.pollInterval == 0 {
		o.pollInterval = d.pollInterval
	}
	if o.progressWindow == 0 {
		o.progressWindow = d.progressWindow
	}
	if o.minCPUDelta == 0 {
		o.minCPUDelta = d.minCPUDelta
	}
	if o.minIODelta == 0 {
		o.minIODelta = d.minIODelta
	}
	if o.msiMutexWait == 0 {
		o.msiMutexWait = d.msiMutexWait
	}
	if o.windowDetector == nil {
		o.windowDetector = d.windowDetector
	}
	return o
}

// resolve merges caller options with process-wide defaults and derives dependent values. It is
// the only place where Unattended is derived.
func resolve(o Options) Options {
	d := CurrentDefaults()
	r := mergeOptions(o, d)
	r.Unattended = o.Unattended || d.Unattended || isSession0()
	r.DisableUIDetection = o.DisableUIDetection || d.DisableUIDetection
	if r.pollInterval < 0 {
		r.pollInterval = defaultPollInterval
	}
	// Never poll more coarsely than a quarter of the shortest enabled watchdog.
	for _, t := range []time.Duration{r.InactivityTimeout, r.UIGracePeriod} {
		if t > 0 && r.pollInterval > t/4 {
			r.pollInterval = t / 4
		}
	}
	if r.pollInterval < 10*time.Millisecond {
		r.pollInterval = 10 * time.Millisecond
	}
	if r.progressWindow <= 0 || (r.InactivityTimeout > 0 && r.progressWindow > r.InactivityTimeout) {
		r.progressWindow = r.InactivityTimeout
	}
	return r
}

// Run executes the command under process tree supervision, monitoring forward progress and UI state.
// Stdout and stderr are tee'd to os.Stdout/os.Stderr and out (if non-nil). Stdin is always disconnected.
//
// On Windows, wusa and DISM commands get the servicing policy described at servicingOptions.
func Run(c *exec.Cmd, opts Options, out io.Writer) error {
	if runtime.GOOS == "windows" && isServicingCommand(c.Path) {
		opts = servicingOptions(opts, adminHardTimeoutConfigured(), os.Getenv("WINDIR"))
	}
	return runSupervised(c, resolve(opts), out)
}
