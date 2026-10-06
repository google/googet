//go:build unix

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
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/logger"
)

// relaySignals are the terminal signals Run catches on Unix.
var relaySignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// startContained starts c in a new process group. kill sends SIGKILL to the
// whole group and exited reports whether c itself has exited. Because the
// group does not receive terminal signals, signals caught by sigs while c runs
// are forwarded to it, and once one was forwarded a later one escalates to
// SIGKILL. Descendants left running after c exits keep running. There is no
// activity counter, so the inactivity limit does not apply.
func startContained(c *exec.Cmd, sigs *signalRelay) (*contained, error) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
	if err := c.Start(); err != nil {
		return nil, err
	}
	// With Setpgid and a zero Pgid the group ID is the child's PID.
	pgid := c.Process.Pid
	// Wait reaps c before draining its output, after which its PID is gone.
	exited := func() bool { return syscall.Kill(pgid, 0) == syscall.ESRCH }
	sigs.relay(func(s os.Signal, acted bool) bool {
		// Descendants that outlive c keep running, unless an earlier signal
		// already interrupted c.
		if !acted && exited() {
			return false
		}
		sig := s.(syscall.Signal)
		if acted {
			// A repeated signal kills an installer that ignores the first, or
			// any descendant still holding its output open.
			sig = syscall.SIGKILL
		}
		// ESRCH means the whole group has already exited.
		if err := syscall.Kill(-pgid, sig); err != nil && err != syscall.ESRCH {
			logger.Errorf("Failed to send %v to %s: %v", sig, c.Path, err)
		}
		return true
	})
	return &contained{
		kill:   func() error { return syscall.Kill(-pgid, syscall.SIGKILL) },
		exited: exited,
	}, nil
}

// Reraise makes the current process die from s, typically the signal returned
// by Interrupted once the caller has cleaned up after Run returned
// ErrInterrupted. If s is nil, Reraise does nothing. Otherwise it restores the
// default action for s and sends s to the process itself; for SIGQUIT the Go
// runtime instead dumps its goroutines and exits with status 2. The signal may
// be handled on another thread, so Reraise then waits a second for it to take
// effect, and returns if it has not.
func Reraise(s os.Signal) {
	if s == nil {
		return
	}
	signal.Reset(s)
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		p.Signal(s)
	}
	time.Sleep(time.Second)
}
