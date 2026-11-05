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
	"os/exec"
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

// Timeout is how long Run lets a command run before killing it and all of its
// descendants. Zero or negative disables the limit. It applies to every
// command Run executes: installers, uninstallers, verify commands and goopack
// build steps. googet sets it from the installtimeout key in googet.conf.
var Timeout = 4 * time.Hour

// ErrInactive is wrapped by the error Run returns when it kills a command that
// went longer than InactivityTimeout without activity.
var ErrInactive = errors.New("command inactive")

// InactivityTimeout is how long Run lets a command go without activity before
// acting according to InactivityMode. Activity is any output and any change in
// the CPU time, I/O or process count of the command's Job Object. Zero or
// negative disables the limit, and positive values below one minute are raised
// to one minute. It only applies on Windows: elsewhere there is no cheap way
// to tell that a quiet command tree is still busy. googet sets it from the
// inactivitytimeout key in googet.conf.
var InactivityTimeout = 5 * time.Minute

// The values of InactivityMode.
const (
	InactivityEnforce = "enforce"
	InactivityMonitor = "monitor"
	InactivityOff     = "off"
)

// InactivityMode is what Run does when a command exceeds InactivityTimeout:
// InactivityEnforce kills it and all of its descendants, InactivityOff
// disables the limit, and InactivityMonitor or any other value logs a warning
// for each stretch of inactivity. The default is InactivityMonitor because
// work the Windows Installer service does for msiexec or wusa runs outside the
// job and is not seen as activity. googet sets it from the inactivitymode key
// in googet.conf.
var InactivityMode = InactivityMonitor

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
// Timeout, in which case the error wraps ErrTimeout, or if it goes longer than
// InactivityTimeout without activity in enforce mode, in which case the error
// wraps ErrInactive.
func Run(c *exec.Cmd, ec []int, w io.Writer) error {
	c.Stdout = io.MultiWriter(progress.Stdout(), w)
	c.Stderr = io.MultiWriter(progress.Stderr(), w)
	c.WaitDelay = waitDelay
	// ErrWaitDelay means the command succeeded but a descendant kept its output
	// open, which is not a failure of the command.
	if err := runContained(c, Timeout); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
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
	kill func()
	// exited reports whether the command itself has exited.
	exited func() bool
	// release is called once the command has been waited for.
	release func()
	// activity returns a counter that grows whenever the command or its
	// descendants do work, or an error if it cannot be read. It is nil where
	// such a counter is unavailable.
	activity func() (uint64, error)
}

// runContained starts c with its descendants contained and waits for it,
// killing them all if c runs longer than a positive timeout or, in enforce
// mode, goes longer than InactivityTimeout without activity.
func runContained(c *exec.Cmd, timeout time.Duration) error {
	// Output counts as activity. MultiWriter flattens c's writers into these.
	var out byteCounter
	c.Stdout = io.MultiWriter(&out, c.Stdout)
	c.Stderr = io.MultiWriter(&out, c.Stderr)
	p, err := startContained(c)
	if err != nil {
		return err
	}
	defer p.release()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	return supervise(c.Path, done, p, timeout, newWatch(p.activity, &out, InactivityTimeout, InactivityMode))
}

// supervise returns the result of waiting for the command, received from done.
// It kills the command through p if it runs longer than a positive timeout or
// if w is not nil and finds it inactive in enforce mode.
func supervise(name string, done <-chan error, p *contained, timeout time.Duration, w *watch) error {
	var deadline, ticks <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	// Without a watch, ticks stays nil and never fires.
	var last uint64
	if w != nil {
		// The baseline is taken before the first tick can arrive.
		last, _ = w.sample()
		if w.ticks == nil {
			t := time.NewTicker(w.interval)
			defer t.Stop()
			w.ticks = t.C
		}
		ticks = w.ticks
	}
	// A command that already exited may still be draining output held open by
	// a descendant; that is neither a timeout nor inactivity.
	stop := func(err error) error {
		if p.exited() {
			return <-done
		}
		p.kill()
		<-done
		return err
	}
	idle, warned, unreadable := 0, false, false
	for {
		select {
		case err := <-done:
			return err
		case <-deadline:
			return stop(fmt.Errorf("%w: %s was killed after %v", ErrTimeout, name, timeout))
		case <-ticks:
			n, err := w.sample()
			if err != nil && !unreadable {
				w.warn("Cannot read the activity of %s, so it always counts as active: %v", name, err)
				unreadable = true
			}
			// A failed sample counts as activity so that a busy command is
			// never killed.
			if err != nil || n != last {
				last, idle, warned = n, 0, false
				continue
			}
			if idle++; time.Duration(idle)*w.interval < w.limit {
				continue
			}
			if !w.monitor {
				return stop(fmt.Errorf("%w: %s was killed after %v without activity", ErrInactive, name, w.limit))
			}
			if !warned && !p.exited() {
				w.warn("%s has had no activity for %v; it would have been killed with inactivitymode %s", name, w.limit, InactivityEnforce)
				warned = true
			}
		}
	}
}

// byteCounter is an io.Writer that counts the bytes written to it.
type byteCounter struct{ n atomic.Uint64 }

func (b *byteCounter) Write(p []byte) (int, error) {
	b.n.Add(uint64(len(p)))
	return len(p), nil
}

// watch is the inactivity watchdog of a command. sample returns a counter
// that grows with any activity, or an error if it could not be read. It is
// called on each tick of ticks, one per interval, and the command is inactive
// once it has not changed for limit. monitor makes inactivity call warn
// instead of killing the command.
type watch struct {
	sample          func() (uint64, error)
	limit, interval time.Duration
	ticks           <-chan time.Time
	monitor         bool
	warn            func(format string, v ...any)
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
func newWatch(activity func() (uint64, error), out *byteCounter, limit time.Duration, mode string) *watch {
	if activity == nil || limit <= 0 || mode == InactivityOff {
		return nil
	}
	if limit < minInactivity {
		raiseLimitWarning.Do(func() {
			logger.Warningf("Raising the inactivity limit of %v to the minimum of %v", limit, minInactivity)
		})
		limit = minInactivity
	}
	return &watch{
		// Both counters only grow, so their sum changes whenever either does.
		sample: func() (uint64, error) {
			n, err := activity()
			return n + out.n.Load(), err
		},
		limit: limit,
		// Sampling about six times per limit keeps the overshoot small.
		interval: max(min(10*time.Second, limit/6), minInterval),
		// Only an explicit enforce kills; anything else, such as a typo, monitors.
		monitor: mode != InactivityEnforce,
		warn:    logger.Warningf,
	}
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
