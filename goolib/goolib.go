/*
Copyright 2016 Google Inc. All Rights Reserved.
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

// Package goolib contains common functions useful when working with GooGet.
package goolib

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/googet/v2/progress"
	"github.com/google/logger"
)

// ErrTimeout is wrapped by the error Run returns when it kills a command that
// ran longer than Timeout.
var ErrTimeout = errors.New("command timed out")

// ErrInterrupted is wrapped by the error Run returns when a signal
// interrupted the command, and by the error every later Run call returns
// without starting its command, including after a signal that arrived while an
// earlier Run was draining output after its command exited.
var ErrInterrupted = errors.New("command interrupted")

// interrupt is process-wide and never cleared by design: once set, no more
// commands run. Tests must trigger it only in subprocesses.
var (
	interruptMu sync.Mutex
	interrupt   os.Signal
)

// Interrupted returns the first signal caught during a Run call, whether it
// interrupted the command or arrived while Run drained output after the
// command exited, or nil if there was none. A caller that stops after an
// interruption can pass it to Reraise once it has cleaned up, so that it still
// dies from the signal.
func Interrupted() os.Signal {
	interruptMu.Lock()
	defer interruptMu.Unlock()
	return interrupt
}

// setInterrupted records s as the signal returned by Interrupted, unless one
// is already recorded.
func setInterrupted(s os.Signal) {
	interruptMu.Lock()
	defer interruptMu.Unlock()
	if interrupt == nil {
		interrupt = s
	}
}

// DefaultTimeout is the default value of Timeout.
const DefaultTimeout = 4 * time.Hour

// Timeout is how long Run lets a command run before killing it and all of its
// descendants. Zero or negative disables the limit. It applies to every
// command Run executes: installers, uninstallers, verify commands and goopack
// build steps. googet sets it from the installtimeout key in googet.conf.
var Timeout = DefaultTimeout

// ErrInactive is wrapped by the error Run returns when it kills a command that
// went longer than InactivityTimeout without activity.
var ErrInactive = errors.New("command inactive")

// InactivityTimeout is how long Run lets a command go without activity before
// acting according to InactivityMode. Activity is any output, and any CPU time,
// I/O or new processes in the command's Job Object. Zero or negative disables
// the limit, and positive values below one minute are raised to one minute. It
// only applies on Windows: elsewhere there is no cheap way to tell that a quiet
// command tree is still busy. googet sets it from the inactivitytimeout key in
// googet.conf.
var InactivityTimeout = 5 * time.Minute

const (
	// InactivityEnforce makes InactivityMode kill an inactive command.
	InactivityEnforce = "enforce"
	// InactivityMonitor makes InactivityMode log warnings without killing.
	InactivityMonitor = "monitor"
	// InactivityOff disables the inactivity watchdog.
	InactivityOff = "off"
)

// InactivityMode is what Run does when a command exceeds InactivityTimeout or
// shows a dialog nobody can answer for InactivityTimeout without I/O.
// InactivityEnforce kills it and all of its descendants, but never while it
// shows a dialog a user can answer, including on a locked console or a
// disconnected RDP session. InactivityOff disables the limit, and
// InactivityMonitor or any other value logs a warning for each stretch of
// inactivity or stuck dialog, and another one if activity resumes. Work done
// outside the command's job is not seen as activity: by the Windows Installer
// service for msiexec or wusa, by descendants that break away from the job,
// and by services, WMI or scheduled tasks the command hands work to.
// InactivityEnforce can then kill the command while that work carries on,
// which is why InactivityMonitor is the default. googet sets it from the
// inactivitymode key in googet.conf.
var InactivityMode = InactivityMonitor

// ErrDialog is wrapped by the error Run returns when it kills a command that
// showed a dialog nobody could answer for InactivityTimeout without I/O.
var ErrDialog = errors.New("command blocked on a dialog")

// dialogGrace, if positive, is how long Run lets a command show a dialog nobody
// can answer before acting according to InactivityMode; otherwise that is the
// effective InactivityTimeout. Tests set it. Nobody can answer when googet runs
// in session 0 or on a window station without a display. Such a dialog counts
// as stuck only while the command does no I/O, writes no output and starts no
// processes. CPU time alone does not count, since the dialog's message loop
// uses some. While a dialog a user can answer is shown, from the first tick
// that sees it, the command is not killed for inactivity, only warned about,
// and the dialog is logged once it has been shown for this long. It only
// applies on Windows, while InactivityTimeout is in effect.
var dialogGrace time.Duration

// isUnattended reports, computed once, whether nobody can answer a dialog
// shown by a command. Only newWatch reads it, so tests can replace it.
var isUnattended = sync.OnceValue(func() bool {
	v, err := unattended()
	if err != nil {
		logger.Warningf("Cannot tell whether dialogs of installers can be answered, so assuming they can: %v", err)
	}
	return v
})

// waitDelay bounds how long Run waits for the command's output to be closed
// after it exits or is killed, since orphaned descendants may hold the pipes
// open indefinitely.
var waitDelay = time.Minute

var interpreter = map[string]string{
	".ps1": "powershell",
	".cmd": "cmd",
	".bat": "cmd",
	".exe": "cmd",
}

// scriptInterpreter reads a scripts extension and returns the interpreter to use.
func scriptInterpreter(s string) (string, error) {
	ext := filepath.Ext(s)
	itp, ok := interpreter[ext]
	if ok {
		return itp, nil
	}
	return "", fmt.Errorf("unknown extension %q", ext)
}

// Exec execs a script or binary on either Windows or Linux using the provided args.
// The process is successful if the exit code matches any of those provided or '0'.
// stdout and stderr are sent to the writer.
func Exec(s string, args []string, ec []int, w io.Writer) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cs := filepath.Clean(s)
		ipr, err := scriptInterpreter(cs)
		if err != nil {
			return err
		}
		switch ipr {
		case "powershell":
			// We are using `-Command` here instead of `-File` as this catches syntax errors in the script.
			args = append([]string{"-ExecutionPolicy", "Bypass", "-NonInteractive", "-NoProfile", "-Command", cs}, args...)
			c = exec.Command(ipr, args...)
		case "cmd":
			c = exec.Command(cs, args...)
		default:
			return fmt.Errorf("unknown interpreter: %q", ipr)
		}
	case "linux":
		c = exec.Command(s, args...)
	default:
		return fmt.Errorf("OS %q is not Windows or Linux", runtime.GOOS)
	}
	return Run(c, ec, w)
}

// Run runs a command.
// The process is successful if the exit code matches any of those provided or '0'.
// stdout and stderr are sent to the writer and to this process's stdout and
// stderr. While a progress spinner is active they are still shown as they are
// produced, a line at a time after clearing the spinner line; nothing is
// withheld or discarded.
//
// The command and its descendants are contained (a Job Object on Windows, a
// process group elsewhere) and are all killed if the command runs longer than
// Timeout, in which case the error wraps ErrTimeout, or if in enforce mode it
// goes longer than InactivityTimeout without activity, in which case the error
// wraps ErrInactive, or shows a dialog nobody can answer for InactivityTimeout
// without I/O, in which case the error wraps ErrDialog.
//
// On Unix the command's process group does not receive terminal signals, so
// SIGINT, SIGTERM, SIGHUP and SIGQUIT that this process receives while the
// command runs are forwarded to the group, unless they were ignored; a second
// one kills the group.
//
// On Windows the first Ctrl-C or Ctrl-Break (os.Interrupt) that this process
// receives while the command runs terminates the job at once, and each later
// one tries again. A console command received the event too but gets no grace
// period, and a GUI installer gets no console events at all. Console close,
// logoff and shutdown events are not caught: Windows ends the process a few
// seconds after them, too soon to finish a cleanup reliably. Outside Run,
// Ctrl-C still ends this process at once.
//
// Once an interrupted command has exited, Run returns an error wrapping
// ErrInterrupted rather than letting the signal end this process, so that the
// caller can clean up, and every later call returns such an error at once
// without starting its command. A signal that arrives once the command itself
// has exited, while Run still waits for output held open by a descendant, is
// not passed on: the descendants keep running and Run returns the command's
// own result, but later calls still fail. Callers must pass Interrupted to
// Reraise after cleanup to still exit from the signal.
func Run(c *exec.Cmd, ec []int, w io.Writer) error {
	if err := interruptedError(c); err != nil {
		return err
	}
	c.Stdout = io.MultiWriter(progress.Stdout(), w)
	c.Stderr = io.MultiWriter(progress.Stderr(), w)
	c.WaitDelay = waitDelay
	err := runContained(c, Timeout)
	// ErrWaitDelay means the command succeeded but a descendant kept its output
	// open, which is not a failure of the command.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		e, ok := err.(*exec.ExitError)
		if !ok {
			return err
		}
		s, ok := e.Sys().(syscall.WaitStatus)
		if !ok {
			return err
		}
		if !slices.Contains(ec, s.ExitStatus()) {
			return fmt.Errorf("command exited with error code %v", s.ExitStatus())
		}
	}
	return nil
}

// contained controls a command started by startContained.
type contained struct {
	// kill kills the command and all of its descendants.
	kill func() error
	// exited reports whether the command itself has exited.
	exited func() bool
	// release is called once the command has been waited for; it may be nil.
	release func()
	// activity returns two counters that grow whenever the command or its
	// descendants use CPU time or do other work, such as I/O or starting
	// processes, or an error if they cannot be read. It is nil where such
	// counters are unavailable.
	activity func() (cpu, io uint64, err error)
	// dialog returns the title of a dialog of the command or its descendants,
	// and whether there is one, given whether nobody can answer it, which
	// decides whether hidden dialogs count. It is nil where windows cannot be
	// listed.
	dialog func(unattended bool) (title string, ok bool)
}

// runContained starts c with its descendants contained and waits for it,
// killing them all if c runs longer than a positive timeout or, in enforce
// mode, goes longer than InactivityTimeout without activity or shows a dialog
// nobody can answer without doing I/O. If a signal reached the command, the
// error wraps ErrInterrupted.
func runContained(c *exec.Cmd, timeout time.Duration) (err error) {
	// Output counts as activity. MultiWriter flattens c's writers into these.
	var out byteCounter
	c.Stdout = io.MultiWriter(&out, c.Stdout)
	c.Stderr = io.MultiWriter(&out, c.Stderr)
	// Signals are caught from before the start so that none is missed.
	sigs := catchSignals(relaySignals...)
	var p *contained
	defer func() {
		// Stop the relay before release closes handles it may still use.
		first, acted := sigs.stop()
		setInterrupted(first)
		if p != nil && p.release != nil {
			p.release()
		}
		if acted {
			err = interruptedError(c)
		}
	}()
	p, err = startContained(c, sigs)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	return supervise(c.Path, done, p, timeout, newWatch(p.activity, &out, InactivityTimeout, InactivityMode))
}

// supervise returns the result of waiting for the command, received from done.
// It kills the command through p if it runs longer than a positive timeout or
// if w is not nil and, in enforce mode, finds it inactive or showing a dialog
// nobody can answer without doing I/O.
func supervise(name string, done <-chan error, p *contained, timeout time.Duration, w *watch) error {
	var deadline, ticks <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	var s *watchState
	// Without a watch, ticks stays nil and never fires.
	if w != nil {
		// The baseline is taken before the first tick can arrive. If it cannot
		// be read, the first tick either reports the error or sees a change,
		// and counts as active either way.
		cpu, io, _ := w.sample()
		s = &watchState{w: w, name: name, lastCPU: cpu, lastIO: io}
		ticks = w.ticks
		if ticks == nil {
			t := time.NewTicker(w.interval)
			defer t.Stop()
			ticks = t.C
		}
	}
	// A command that already exited may still be draining output held open by
	// a descendant; that is neither a timeout nor inactivity.
	stop := func(err error) error {
		if p.exited() {
			return <-done
		}
		if kerr := p.kill(); kerr != nil {
			logger.Errorf("Failed to kill %s: %v", name, kerr)
		}
		<-done
		return err
	}
	for {
		select {
		case err := <-done:
			return err
		case <-deadline:
			return stop(fmt.Errorf("%w: %s was killed after %v", ErrTimeout, name, timeout))
		case <-ticks:
			o := observation{unattended: w.unattended()}
			o.cpu, o.io, o.err = w.sample()
			if p.dialog != nil {
				o.title, o.dialog = p.dialog(o.unattended)
			}
			o.exited = p.exited()
			for _, d := range s.step(o) {
				switch d.action {
				case actKill:
					return stop(d.error())
				case actInfo:
					w.info(d.format, d.args...)
				case actWarn, actResume:
					w.warn(d.format, d.args...)
				}
			}
		}
	}
}

// observation is what supervise sees on one tick of a watch: the activity
// samples or the error reading them, the title of a dialog if the command
// shows one, whether nobody can answer it, and whether the command itself has
// exited.
type observation struct {
	cpu, io            uint64
	err                error
	title              string
	dialog, unattended bool
	exited             bool
}

// action is what a decision asks supervise to do.
type action int

const (
	// actWarn logs a warning.
	actWarn action = iota
	// actInfo logs an informational message.
	actInfo
	// actResume logs a warning that activity resumed after a stretch that was
	// warned about.
	actResume
	// actKill kills the command, which then fails with the decision's error.
	actKill
)

// decision is one thing supervise does after a tick. format and args describe
// the message, or the kill after err, a sentinel error.
type decision struct {
	action action
	err    error
	format string
	args   []any
}

// error returns the error a command killed by d fails with.
func (d decision) error() error {
	return fmt.Errorf("%w: %s", d.err, fmt.Sprintf(d.format, d.args...))
}

// watchState is what supervise remembers between ticks of the watch w of the
// command name: the last samples, the number of ticks since they changed,
// whether that stretch of inactivity was warned about, whether a failure to
// read the activity was, and the current dialog stretch.
type watchState struct {
	w                  *watch
	name               string
	lastCPU, lastIO    uint64
	idle               int
	warned, unreadable bool
	dialogStretch
}

// dialogStretch tracks a dialog across ticks: the number of ticks in a row
// that saw it without I/O, whether it was acted on, and whether a user could
// answer it on the last tick.
type dialogStretch struct {
	dialogSeen                   int
	dialogHandled, wasAnswerable bool
}

// step updates s with the observation of one tick and returns what supervise
// must do about it, in order; a kill comes last. It neither logs nor kills, so
// that tests can drive it tick by tick.
func (s *watchState) step(o observation) []decision {
	var ds []decision
	if o.err != nil && !s.unreadable {
		ds = append(ds, decision{action: actWarn, format: "Cannot read the activity of %s, so it always counts as active: %v", args: []any{s.name, o.err}})
		s.unreadable = true
	}
	// A failed sample counts as activity so that a busy command is never
	// killed.
	ioChanged := o.err != nil || o.io != s.lastIO
	active := ioChanged || o.cpu != s.lastCPU
	s.lastCPU, s.lastIO = o.cpu, o.io
	answerable := o.dialog && !o.unattended
	// Dismissing a dialog may use negligible CPU and no I/O, so start a fresh
	// inactivity window once a user-answerable dialog closes. On an active tick
	// the activity does that, and logs the end of a warned stretch.
	if s.wasAnswerable && !answerable && !active {
		s.idle, s.warned = 0, false
	}
	s.wasAnswerable = answerable
	// A dialog's message loop uses CPU time, so a command showing one counts
	// as stuck as long as it does no I/O.
	if o.dialog && !ioChanged {
		s.dialogSeen++
	} else {
		s.dialogSeen, s.dialogHandled = 0, false
	}
	if !s.dialogHandled && time.Duration(s.dialogSeen)*s.w.interval >= s.w.grace && !o.exited {
		s.dialogHandled = true
		switch {
		case answerable:
			ds = append(ds, decision{action: actInfo, format: "%s shows %q, which a user can answer", args: []any{s.name, o.title}})
		case !s.w.monitor:
			return append(ds, decision{action: actKill, err: ErrDialog, format: "%s was killed after showing a dialog for %v: %q", args: []any{s.name, s.w.grace, o.title}})
		default:
			ds = append(ds, decision{action: actWarn, format: "%s has shown %q for %v with nobody to answer it; it would have been killed with inactivitymode %s", args: []any{s.name, o.title, s.w.grace, InactivityEnforce}})
		}
	}
	if active {
		// Telling a slow command from a hung one needs both ends of a stretch
		// that was warned about. A failed sample is not known to be activity.
		if s.warned && o.err == nil {
			ds = append(ds, decision{action: actResume, format: "%s is active again after %v without activity", args: []any{s.name, time.Duration(s.idle) * s.w.interval}})
		}
		s.idle, s.warned = 0, false
		return ds
	}
	s.idle++
	if time.Duration(s.idle)*s.w.interval < s.w.limit || o.exited {
		return ds
	}
	// A user may take their time to answer a dialog.
	if !s.w.monitor && !answerable {
		return append(ds, decision{action: actKill, err: ErrInactive, format: "%s was killed after %v without activity", args: []any{s.name, s.w.limit}})
	}
	if !s.warned {
		if answerable {
			ds = append(ds, decision{action: actWarn, format: "%s has had no activity for %v but shows %q, which a user can answer", args: []any{s.name, s.w.limit, o.title}})
		} else {
			ds = append(ds, decision{action: actWarn, format: "%s has had no activity for %v; it would have been killed with inactivitymode %s", args: []any{s.name, s.w.limit, InactivityEnforce}})
		}
		s.warned = true
	}
	return ds
}

// byteCounter is an io.Writer that counts the bytes written to it.
type byteCounter struct{ n atomic.Uint64 }

func (b *byteCounter) Write(p []byte) (int, error) {
	b.n.Add(uint64(len(p)))
	return len(p), nil
}

// watch is the inactivity watchdog of a command. sample returns two counters
// that grow with the command's CPU time and with its other activity, or an
// error if they could not be read. It is called on each tick of ticks, one per
// interval, and the command is inactive once neither has changed for limit.
// monitor makes inactivity call warn instead of killing the command. grace is
// how long a dialog may be shown without I/O before it is acted on. info logs
// dialogs that a user can answer. unattended reports whether nobody can answer
// a dialog; supervise calls it once per tick.
type watch struct {
	sample                 func() (cpu, io uint64, err error)
	limit, interval, grace time.Duration
	ticks                  <-chan time.Time
	monitor                bool
	warn, info             func(format string, v ...any)
	unattended             func() bool
}

// minInactivity and minInterval bound the inactivity limit and the sampling
// interval from below. Windows charges CPU time in clock ticks of about 15.6ms,
// so a busy command can show no change over very short periods. Tests lower
// them.
var minInactivity, minInterval = time.Minute, time.Second

// raiseLimitWarning makes newWatch warn only once about raising a short limit.
var raiseLimitWarning sync.Once

// newWatch returns a watch that counts both the output written to out and
// activity as activity, or nil if activity is nil, limit is not positive or
// mode is InactivityOff. Output alone is not enough because installers can be
// busy for a long time without writing anything.
func newWatch(activity func() (cpu, io uint64, err error), out *byteCounter, limit time.Duration, mode string) *watch {
	if activity == nil || limit <= 0 || mode == InactivityOff {
		return nil
	}
	if limit < minInactivity {
		raiseLimitWarning.Do(func() {
			logger.Warningf("Raising the inactivity limit of %v to the minimum of %v", limit, minInactivity)
		})
		limit = minInactivity
	}
	grace := dialogGrace
	if grace <= 0 {
		grace = limit
	}
	return &watch{
		// Both counters only grow, so their sum changes whenever either does.
		sample: func() (uint64, uint64, error) {
			cpu, n, err := activity()
			return cpu, n + out.n.Load(), err
		},
		limit: limit,
		grace: grace,
		// Sampling about six times per limit keeps the overshoot small.
		interval: max(min(10*time.Second, limit/6), minInterval),
		// Only an explicit enforce kills; anything else, such as a typo, monitors.
		monitor:    mode != InactivityEnforce,
		warn:       logger.Warningf,
		info:       logger.Infof,
		unattended: isUnattended,
	}
}

// interruptedError returns an error wrapping ErrInterrupted if Interrupted is
// non-nil, and nil otherwise.
func interruptedError(c *exec.Cmd) error {
	if s := Interrupted(); s != nil {
		return fmt.Errorf("%w: %s (%v)", ErrInterrupted, c.Path, s)
	}
	return nil
}

// signalRelay catches signals while one contained command runs.
type signalRelay struct {
	sigs       chan os.Signal
	firstCh    chan struct{}
	beforeStop func()
	// done is closed once the goroutine started by relay has handled every
	// caught signal; first is the first of them, and acted reports whether
	// handle acted on any. Both are read only by stop after done.
	done  chan struct{}
	first os.Signal
	acted bool
}

// catchSignals starts catching sigs. Ignored ones, as under nohup, are left
// alone so the command still inherits that.
func catchSignals(sigs ...os.Signal) *signalRelay {
	r := &signalRelay{
		sigs:    make(chan os.Signal, 1),
		firstCh: make(chan struct{}),
	}
	for _, s := range sigs {
		if !signal.Ignored(s) {
			signal.Notify(r.sigs, s)
		}
	}
	return r
}

// relay calls handle for each caught signal, in a new goroutine, until stop is
// called. acted reports whether handle already acted on an earlier signal, and
// handle reports whether it acted on s.
func (r *signalRelay) relay(handle func(s os.Signal, acted bool) bool) {
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		for s := range r.sigs {
			if r.first == nil {
				r.first = s
				close(r.firstCh)
			}
			if handle(s, r.acted) {
				r.acted = true
			}
		}
	}()
}

// waitFirst waits up to d for the relay to receive a signal.
func (r *signalRelay) waitFirst(d time.Duration) {
	if r.done == nil {
		return
	}
	select {
	case <-r.firstCh:
	case <-time.After(d):
	}
}

// stop stops catching signals and returns the first one caught, or nil, and
// whether handle acted on any. If relay was called, it first waits until
// every caught signal was handled.
func (r *signalRelay) stop() (os.Signal, bool) {
	if r.beforeStop != nil {
		r.beforeStop()
	}
	// No more signals are sent to sigs once Stop returns.
	signal.Stop(r.sigs)
	close(r.sigs)
	if r.done == nil {
		// Without relay, at most one signal is buffered.
		return <-r.sigs, false
	}
	<-r.done
	return r.first, r.acted
}

// PackageInfo describes the name arch and version of a package.
type PackageInfo struct {
	Name, Arch, Ver string
}

func (pi PackageInfo) String() string {
	if pi.Arch != "" && pi.Ver != "" {
		return fmt.Sprintf("%s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	}
	if pi.Arch != "" {
		return fmt.Sprintf("%s.%s", pi.Name, pi.Arch)
	}
	return pi.Name
}

// PkgName returns the proper goo package name.
func (pi PackageInfo) PkgName() string {
	return fmt.Sprintf("%s.%s.%s.goo", pi.Name, pi.Arch, pi.Ver)
}

// PkgNameSplit returns the PackageInfo from a package name.
// If the package name does not contain arch or version an empty string
// will be returned.
func PkgNameSplit(pn string) PackageInfo {
	pi := strings.SplitN(strings.TrimSpace(pn), ".", 3)
	if len(pi) == 2 {
		return PackageInfo{pi[0], pi[1], ""}
	}
	if len(pi) == 3 {
		return PackageInfo{pi[0], pi[1], pi[2]}
	}
	return PackageInfo{pi[0], "", ""}
}

// Checksum retuns the SHA256 checksum of the provided reader.
func Checksum(r io.Reader) string {
	hash := sha256.New()
	io.Copy(hash, r)
	return hex.EncodeToString(hash.Sum(nil))
}

// ExtractPkgSpec pulls and unmarshals the package spec file from a
// reader.
func ExtractPkgSpec(r io.Reader) (*PkgSpec, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	return ReadPackageSpec(zr)
}

// SplitGCSUrl parses and splits a GCS URL returning if the URL belongs to a GCS object,
// and if so the bucket and object.
// Code modified from https://github.com/GoogleCloudPlatform/compute-image-tools/blob/master/daisy/storage.go
func SplitGCSUrl(p string) (bool, string, string) {
	bucket := `([a-z0-9][-_.a-z0-9]*)`
	object := `(/(?U)(.+)/*)?`
	bucketRegex := regexp.MustCompile(fmt.Sprintf(`^gs://%s/?$`, bucket))
	gsRegex := regexp.MustCompile(fmt.Sprintf(`^gs://%s%s$`, bucket, object))
	gsHTTPRegex1 := regexp.MustCompile(fmt.Sprintf(`^http[s]?://%s\.(?i:storage\.googleapis\.com)%s$`, bucket, object))
	gsHTTPRegex2 := regexp.MustCompile(fmt.Sprintf(`^http[s]?://(?i:storage\.cloud\.google\.com)/%s%s$`, bucket, object))
	gsHTTPRegex3 := regexp.MustCompile(fmt.Sprintf(`^http[s]?://(?i:(?:commondata)?storage\.googleapis\.com)/%s%s$`, bucket, object))

	for _, rgx := range []*regexp.Regexp{gsRegex, gsHTTPRegex1, gsHTTPRegex2, gsHTTPRegex3} {
		matches := rgx.FindStringSubmatch(p)
		if matches != nil {
			return true, matches[1], matches[3]
		}
	}

	matches := bucketRegex.FindStringSubmatch(p)
	if matches != nil {
		return true, matches[1], ""
	}

	return false, "", ""
}
