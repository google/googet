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
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv selects what the test binary does when executed as a helper:
// "run", "run-stubborn", "run-stubborn-child", "run-stubborn-detached",
// "run-nohup", "run-exit" and "run-exit-stubborn" act as googet (see
// actAsGooget); "hang" starts a grandchild, prints its PID and hangs;
// "stubborn" does the same while it and its grandchild ignore SIGTERM and
// os.Interrupt, "stubborn-child" leaves only its grandchild ignoring them, and
// "stubborn-detached" also leaves the grandchild's stdout closed; "nohup" does
// the same while its grandchild catches SIGHUP; "exit" and "fail" do the same
// but exit at once with code 0 or 3, and "exit-stubborn" does what "stubborn"
// does but exits at once with code 0; "sleep", "sleep-stubborn" and
// "sleep-hup" are the grandchild; "true" exits at once; "spin" uses CPU
// without output for spinFor and exits; "dialog", "hidden-helper-window" and
// "spawn-spin" are Windows-only (see platformHelper).
const helperEnv = "GOOLIB_TEST_HELPER"

// runReturned is printed to stderr by actAsGooget once Run has behaved as
// expected, just before it calls Reraise.
const runReturned = "Run returned as expected"

// spinFor is how long the "spin" helper runs.
const spinFor = 3 * time.Second

// helperCmd returns a command that runs the test binary in the given helper
// mode. The race detector's exit delay is disabled so helpers exit promptly.
func helperCmd(mode string) *exec.Cmd {
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), helperEnv+"="+mode, "GORACE=atexit_sleep_ms=0")
	return c
}

// stubborn makes os.Interrupt harmless to the current process. It catches the
// signal rather than ignoring it, because Windows ends a process that ignores
// Ctrl-C or Ctrl-Break.
func stubborn() {
	signal.Notify(make(chan os.Signal, 1), os.Interrupt)
}

// actAsGooget calls Run on the installer selected by mode (see helperEnv).
// Hanging modes expect Run to return ErrInterrupted when signaled; "run-exit"
// and "run-exit-stubborn" are signaled after the installer has exited while
// its grandchild holds output open, and expect Run to return nil. In every
// mode a second Run must return ErrInterrupted without starting its command,
// after which actAsGooget prints runReturned and calls Reraise as googet does.
// It exits with code 4 if a check fails.
func actAsGooget(mode string) {
	installer, wantErr := "hang", ErrInterrupted
	switch mode {
	case "run-stubborn", "run-stubborn-child", "run-stubborn-detached":
		installer = strings.TrimPrefix(mode, "run-")
	case "run-nohup":
		signal.Ignore(syscall.SIGHUP)
		installer = "nohup"
	case "run-exit", "run-exit-stubborn":
		installer, wantErr = strings.TrimPrefix(mode, "run-"), nil
		// Run waits this long for the grandchild, which leaves time to signal.
		waitDelay = 5 * time.Second
	}
	if err := Run(helperCmd(installer), nil, io.Discard); !errors.Is(err, wantErr) {
		fmt.Fprintf(os.Stderr, "Run() error = %v, want %v\n", err, wantErr)
		os.Exit(4)
	}
	c := helperCmd("true")
	if err := Run(c, nil, io.Discard); !errors.Is(err, ErrInterrupted) || c.Process != nil {
		fmt.Fprintf(os.Stderr, "second Run() error = %v, started = %v, want %v without starting\n", err, c.Process != nil, ErrInterrupted)
		os.Exit(4)
	}
	fmt.Fprintln(os.Stderr, runReturned)
	Reraise(Interrupted())
	os.Exit(0)
}

func TestMain(m *testing.M) {
	switch mode := os.Getenv(helperEnv); mode {
	case "run", "run-stubborn", "run-stubborn-child", "run-stubborn-detached", "run-nohup", "run-exit", "run-exit-stubborn":
		actAsGooget(mode)
	case "hang", "stubborn", "stubborn-child", "stubborn-detached", "nohup", "exit", "exit-stubborn", "fail":
		gcMode := "sleep"
		switch mode {
		case "stubborn", "exit-stubborn":
			// Go keeps an inherited SIG_IGN only for SIGHUP and SIGINT, so the
			// grandchild ignores SIGTERM itself. Both also survive Windows
			// Ctrl-C and Ctrl-Break, as a GUI installer does (see stubborn).
			signal.Ignore(syscall.SIGTERM)
			stubborn()
			gcMode = "sleep-stubborn"
		case "stubborn-child", "stubborn-detached":
			gcMode = "sleep-stubborn"
		case "nohup":
			gcMode = "sleep-hup"
		}
		// The grandchild inherits stdout unless detached, so it holds Run's
		// pipe open too.
		gc := helperCmd(gcMode)
		if mode != "stubborn-detached" {
			gc.Stdout = os.Stdout
		}
		var ready io.Reader
		if gcMode != "sleep" {
			var err error
			ready, err = gc.StderrPipe()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		if err := gc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if ready != nil {
			// The test signals as soon as the PID is printed, so wait until the
			// grandchild has set its signal disposition.
			if _, err := io.ReadFull(ready, make([]byte, 1)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		fmt.Println(gc.Process.Pid)
		switch mode {
		case "exit", "exit-stubborn":
			os.Exit(0)
		case "fail":
			os.Exit(3)
		}
		go gc.Wait()
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	case "sleep", "sleep-stubborn", "sleep-hup":
		switch mode {
		case "sleep-stubborn":
			signal.Ignore(syscall.SIGTERM)
			stubborn()
			os.Stderr.Write([]byte{'\n'})
		case "sleep-hup":
			// signal.Reset leaves an inherited SIG_IGN in place; Notify
			// overrides it so a mistakenly forwarded SIGHUP ends the grandchild.
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGHUP)
			os.Stderr.Write([]byte{'\n'})
			<-ch
			os.Exit(1)
		}
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	case "true":
		os.Exit(0)
	case "spin":
		for end := time.Now().Add(spinFor); time.Now().Before(end); {
			// Burn CPU without output.
		}
		os.Exit(0)
	case "":
	default:
		platformHelper(mode)
		fmt.Fprintf(os.Stderr, "unknown %s mode %q\n", helperEnv, mode)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// startGooget starts the test binary in the given actAsGooget mode with stderr
// written to stderr and the given process attributes, which may be nil, and
// waits for its installer to print the grandchild PID. The helper is killed
// when the test finishes if still running.
func startGooget(t *testing.T, mode string, stderr io.Writer, attr *syscall.SysProcAttr) (*exec.Cmd, int) {
	t.Helper()
	h := helperCmd(mode)
	h.Stderr = stderr
	h.SysProcAttr = attr
	out, err := h.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := h.Start(); err != nil {
		t.Fatalf("starting %s helper: %v", mode, err)
	}
	line, rerr := bufio.NewReader(out).ReadString('\n')
	pid, perr := strconv.Atoi(strings.TrimSpace(line))
	if rerr != nil || perr != nil {
		h.Process.Kill()
		h.Wait()
		t.Fatalf("%s helper printed %q (read error: %v), want a grandchild PID", mode, line, rerr)
	}
	t.Cleanup(func() {
		h.Process.Kill()
	})
	return h, pid
}

// runHelper runs the test binary in the given helper mode through Run with
// the given timeout and accepted exit codes. It returns the PID of the
// grandchild, which is killed at the end of the test if still running, how
// long Run took and its error.
func runHelper(t *testing.T, mode string, timeout time.Duration, ec []int) (pid int, elapsed time.Duration, err error) {
	t.Helper()
	orig := Timeout
	t.Cleanup(func() { Timeout = orig })
	Timeout = timeout
	var w syncBuffer
	start := time.Now()
	err = Run(helperCmd(mode), ec, &w)
	elapsed = time.Since(start)
	pid, perr := strconv.Atoi(strings.TrimSpace(w.String()))
	if perr != nil {
		t.Fatalf("Run(%s helper) printed %q, want a grandchild PID (Run error: %v)", mode, w.String(), err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil && processAlive(pid) {
			p.Kill()
		}
	})
	return pid, elapsed, err
}

// setWaitDelay sets waitDelay to d for the duration of the test.
func setWaitDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := waitDelay
	t.Cleanup(func() { waitDelay = orig })
	waitDelay = d
}

// waitGone reports whether process pid exits within d.
func waitGone(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); processAlive(pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

func TestRunTimeoutKillsTree(t *testing.T) {
	const timeout = 3 * time.Second
	pid, elapsed, err := runHelper(t, "hang", timeout, nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run() error = %v, want %v", err, ErrTimeout)
	}
	// A grandchild left holding the output pipe would delay Run by waitDelay.
	if elapsed > timeout+5*time.Second {
		t.Errorf("Run() took %v, want it to return promptly after the %v timeout", elapsed, timeout)
	}
	if !waitGone(pid, 5*time.Second) {
		t.Errorf("grandchild %d is still running after the timeout", pid)
	}
}

func TestRunNoTimeoutLeavesDescendants(t *testing.T) {
	setWaitDelay(t, 200*time.Millisecond)
	// The grandchild keeps the output pipe open after the command exits, which
	// must not block Run or count as a failure.
	pid, elapsed, err := runHelper(t, "exit", 0, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Run() took %v, want it bounded by waitDelay", elapsed)
	}
	// Descendants that outlive a normal exit keep running, as before.
	if !processAlive(pid) {
		t.Errorf("grandchild %d was killed after a normal exit", pid)
	}
}

func TestRunTimeoutWhileDrainingOutput(t *testing.T) {
	// The command exits at once, but the timeout fires while Run still waits
	// for the grandchild to close the output pipe.
	setWaitDelay(t, 2*time.Second)
	pid, elapsed, err := runHelper(t, "exit", time.Second, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if elapsed < time.Second {
		t.Errorf("Run() returned after %v, before the timeout fired", elapsed)
	}
	if !processAlive(pid) {
		t.Errorf("grandchild %d was killed although the command exited before the timeout", pid)
	}
}

func TestRunExitCodeWhileDrainingOutput(t *testing.T) {
	setWaitDelay(t, 200*time.Millisecond)
	for _, tt := range []struct {
		name    string
		ec      []int
		wantErr string
	}{
		{"rejected", nil, "command exited with error code 3"},
		{"accepted", []int{3}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := runHelper(t, "fail", time.Hour, tt.ec)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tt.wantErr {
				t.Errorf("Run() error = %q, want %q", got, tt.wantErr)
			}
		})
	}
}

func TestReraiseNil(t *testing.T) {
	start := time.Now()
	Reraise(nil)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Reraise(nil) took %v, want immediate return", elapsed)
	}
}

func TestSignalRelay(t *testing.T) {
	// The relays catch no signals and are fed directly, so that no signal
	// reaches the test process.
	t.Run("relayed", func(t *testing.T) {
		r := catchSignals()
		type call struct {
			s     os.Signal
			acted bool
		}
		// handle runs in the relay goroutine; stop orders it before the check.
		var got []call
		r.relay(func(s os.Signal, acted bool) bool {
			got = append(got, call{s, acted})
			// Ignore SIGHUP so acted stays false until SIGTERM is handled.
			return s != syscall.SIGHUP
		})
		r.sigs <- syscall.SIGHUP
		r.sigs <- syscall.SIGTERM
		r.sigs <- os.Interrupt
		s, acted := r.stop()
		if s != syscall.SIGHUP || !acted {
			t.Errorf("stop() = %v, %v, want %v, true", s, acted, syscall.SIGHUP)
		}
		want := []call{{syscall.SIGHUP, false}, {syscall.SIGTERM, false}, {os.Interrupt, true}}
		if !slices.Equal(got, want) {
			t.Errorf("handled %v, want %v", got, want)
		}
	})
	t.Run("not acted on", func(t *testing.T) {
		r := catchSignals()
		r.relay(func(os.Signal, bool) bool { return false })
		r.sigs <- os.Interrupt
		// The signal is still recorded.
		if s, acted := r.stop(); s != os.Interrupt || acted {
			t.Errorf("stop() = %v, %v, want %v, false", s, acted, os.Interrupt)
		}
	})
	t.Run("beforeStop waits for signal", func(t *testing.T) {
		r := catchSignals()
		r.relay(func(os.Signal, bool) bool { return true })
		r.beforeStop = func() {
			go func() {
				time.Sleep(20 * time.Millisecond)
				r.sigs <- os.Interrupt
			}()
			r.waitFirst(time.Second)
		}
		if s, acted := r.stop(); s != os.Interrupt || !acted {
			t.Errorf("stop() = %v, %v, want %v, true", s, acted, os.Interrupt)
		}
	})
	t.Run("before relay", func(t *testing.T) {
		r := catchSignals()
		r.sigs <- os.Interrupt
		if s, acted := r.stop(); s != os.Interrupt || acted {
			t.Errorf("stop() = %v, %v, want %v, false", s, acted, os.Interrupt)
		}
	})
	t.Run("none", func(t *testing.T) {
		r := catchSignals()
		r.relay(func(os.Signal, bool) bool {
			t.Error("handle called without a signal")
			return true
		})
		if s, acted := r.stop(); s != nil || acted {
			t.Errorf("stop() = %v, %v, want nil, false", s, acted)
		}
	})
}
