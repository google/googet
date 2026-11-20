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
// Timeout, in which case the error wraps ErrTimeout.
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

// runContained starts c with its descendants contained and waits for it. If
// timeout is positive and c runs longer, the command and all of its
// descendants are killed. If a signal reached the command, the error wraps
// ErrInterrupted.
func runContained(c *exec.Cmd, timeout time.Duration) (err error) {
	// Signals are caught from before the start so that none is missed.
	sigs := catchSignals(relaySignals...)
	var release func()
	defer func() {
		// Stop the relay before release closes handles it may still use.
		first, acted := sigs.stop()
		setInterrupted(first)
		if release != nil {
			release()
		}
		if acted {
			err = interruptedError(c)
		}
	}()
	var kill func() error
	var exited func() bool
	kill, exited, release, err = startContained(c, sigs)
	if err != nil {
		return err
	}
	if timeout <= 0 {
		return c.Wait()
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		// A command that already exited may still be draining output held open
		// by a descendant; that is not a timeout.
		if exited() {
			return <-done
		}
		if err := kill(); err != nil {
			logger.Errorf("Failed to kill %s after %v: %v", c.Path, timeout, err)
		}
		<-done
		return fmt.Errorf("%w: %s was killed after %v", ErrTimeout, c.Path, timeout)
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
