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
)

// startContained starts c in a new process group. kill sends SIGKILL to the
// whole group and exited reports whether c itself has exited. Because the
// group does not receive terminal signals, SIGINT, SIGTERM and SIGHUP sent to
// googet are forwarded to it, a second one escalates to SIGKILL, and release
// re-raises the first one so that googet still dies from it. Otherwise
// release does nothing, so descendants left running after a normal exit keep
// running.
func startContained(c *exec.Cmd) (kill func(), exited func() bool, release func(), err error) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
	// Signals are caught from before the start so that none is missed. Ignored
	// ones, as under nohup, are left alone so the child still inherits that.
	sigs := make(chan os.Signal, 1)
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			signal.Notify(sigs, s)
		}
	}
	if err := c.Start(); err != nil {
		signal.Stop(sigs)
		select {
		case s := <-sigs:
			reraise(s.(syscall.Signal))
		default:
		}
		return nil, nil, nil, err
	}
	// With Setpgid and a zero Pgid the group ID is the child's PID.
	pgid := c.Process.Pid
	first := make(chan syscall.Signal, 1)
	go func() {
		defer close(first)
		for s := range sigs {
			sig := s.(syscall.Signal)
			select {
			case first <- sig:
			default:
				// A repeated signal kills an installer that ignores the first.
				sig = syscall.SIGKILL
			}
			syscall.Kill(-pgid, sig)
		}
	}()
	release = func() {
		// No more signals are sent to sigs once Stop returns.
		signal.Stop(sigs)
		close(sigs)
		// This yields the first signal, or waits until the forwarding goroutine
		// has drained sigs and closed first, so no signal is missed.
		if s, ok := <-first; ok {
			reraise(s)
		}
	}
	// Wait reaps c before draining its output, after which its PID is gone.
	exited = func() bool { return syscall.Kill(pgid, 0) == syscall.ESRCH }
	return func() { syscall.Kill(-pgid, syscall.SIGKILL) }, exited, release, nil
}

// reraise sends s to googet itself once it is no longer caught. The signal
// may be handled on another thread, so it waits for it to end the process
// rather than let googet carry on.
func reraise(s syscall.Signal) {
	syscall.Kill(os.Getpid(), s)
	time.Sleep(time.Second)
}
