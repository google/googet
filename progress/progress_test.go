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

package progress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeClock is a deterministic replacement for the package-level now.
type fakeClock struct {
	t time.Time
}

// now returns the current fake time.
func (c *fakeClock) now() time.Time { return c.t }

// advance moves the fake time forward by d.
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// setup redirects the package output to fresh buffers, sets enabled and
// installs a fake clock, restoring the package defaults when the test ends.
// The fake clock starts at the real current time so that spinner frames drawn
// from real ticker timestamps still render as 00:00.
func setup(t *testing.T, on bool) (outBuf, stdoutBuf *bytes.Buffer, clock *fakeClock) {
	t.Helper()
	outBuf, stdoutBuf = &bytes.Buffer{}, &bytes.Buffer{}
	clock = &fakeClock{t: time.Now()}
	origWidth := termWidth
	mu.Lock()
	enabled = on
	out = outBuf
	stdout = stdoutBuf
	active = nil
	lastLine = ""
	now = clock.now
	termWidth = func() int { return 0 }
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		enabled = false
		out = os.Stderr
		stdout = os.Stdout
		active = nil
		lastLine = ""
		now = time.Now
		termWidth = origWidth
	})
	return outBuf, stdoutBuf, clock
}

// currentLastLen returns the length of the currently rendered line under the package lock.
func currentLastLen() int {
	mu.Lock()
	defer mu.Unlock()
	return len(lastLine)
}

// currentActive returns active under the package lock.
func currentActive() *Spinner {
	mu.Lock()
	defer mu.Unlock()
	return active
}

// snapshot returns the contents of buf under the package lock, which makes it
// safe to call while a spinner goroutine may be redrawing.
func snapshot(buf *bytes.Buffer) string {
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}

// reset empties buf under the package lock.
func reset(buf *bytes.Buffer) {
	mu.Lock()
	defer mu.Unlock()
	buf.Reset()
}

// barLine renders the expected bar line for the given progress.
func barLine(eq int, pct int, cur, total string, elapsed string) string {
	return fmt.Sprintf("\r|%s%s| %3d%%  %s / %s [%s]",
		strings.Repeat("=", eq), strings.Repeat("-", barWidth-eq), pct, cur, total, elapsed)
}

func TestInit(t *testing.T) {
	origIsTerminal := isTerminal
	t.Cleanup(func() {
		isTerminal = origIsTerminal
		mu.Lock()
		enabled = false
		mu.Unlock()
	})
	for _, tc := range []struct {
		desc     string
		allow    bool
		terminal bool
		term     string
		want     bool
	}{
		{desc: "terminal", allow: true, terminal: true, term: "xterm-256color", want: true},
		{desc: "not allowed", allow: false, terminal: true, term: "xterm-256color", want: false},
		{desc: "not a terminal", allow: true, terminal: false, term: "xterm-256color", want: false},
		{desc: "dumb terminal", allow: true, terminal: true, term: "dumb", want: false},
		{desc: "no TERM", allow: true, terminal: true, term: "", want: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			isTerminal = func(*os.File) bool { return tc.terminal }
			t.Setenv("TERM", tc.term)
			Init(tc.allow)
			if got := Enabled(); got != tc.want {
				t.Errorf("Init(%v) with terminal=%v TERM=%q: Enabled() = %v, want %v", tc.allow, tc.terminal, tc.term, got, tc.want)
			}
		})
	}
}

func TestDisabled(t *testing.T) {
	outBuf, stdoutBuf, _ := setup(t, false)

	if Enabled() {
		t.Error("Enabled() = true, want false")
	}
	b := NewBar("Title", 100, 0)
	if b != nil {
		t.Errorf("NewBar() = %v, want nil", b)
	}
	if n, err := b.Write([]byte("12345")); n != 5 || err != nil {
		t.Errorf("nil Bar.Write() = %d, %v, want 5, nil", n, err)
	}
	b.Finish()
	b.Abort()

	s := NewSpinner("Title")
	if s != nil {
		t.Errorf("NewSpinner() = %v, want nil", s)
	}
	s.Stop(nil)
	s.Stop(errors.New("x"))

	if got := outBuf.String(); got != "" {
		t.Errorf("out = %q, want empty", got)
	}

	Printf("hello %d\n", 1)
	if got, want := stdoutBuf.String(), "hello 1\n"; got != want {
		t.Errorf("stdout after Printf = %q, want %q", got, want)
	}
	if got := outBuf.String(); got != "" {
		t.Errorf("out after Printf = %q, want empty", got)
	}
}

func TestBarKnownTotal(t *testing.T) {
	outBuf, _, clock := setup(t, true)

	b := NewBar("Title", 100, 0)
	if b == nil {
		t.Fatal("NewBar() = nil, want non-nil")
	}
	if got, want := outBuf.String(), "Title (100 B)\n"+barLine(0, 0, "0 B", "100 B", "00:00"); got != want {
		t.Errorf("initial output = %q, want %q", got, want)
	}

	outBuf.Reset()
	clock.advance(redrawInterval)
	if n, err := b.Write(make([]byte, 50)); n != 50 || err != nil {
		t.Errorf("Write() = %d, %v, want 50, nil", n, err)
	}
	if got, want := outBuf.String(), barLine(17, 50, "50 B", "100 B", "00:00"); got != want {
		t.Errorf("output at 50%% = %q, want %q", got, want)
	}
	if !strings.Contains(outBuf.String(), " 50%") {
		t.Errorf("output at 50%% = %q, want it to contain %q", outBuf.String(), " 50%")
	}

	outBuf.Reset()
	clock.advance(time.Second)
	b.Finish()
	if got, want := outBuf.String(), barLine(35, 100, "100 B", "100 B", "00:01")+"\n"; got != want {
		t.Errorf("output after Finish = %q, want %q", got, want)
	}
	if got := currentLastLen(); got != 0 {
		t.Errorf("lastLen after Finish = %d, want 0", got)
	}
}

func TestBarFinishCapsOverrun(t *testing.T) {
	outBuf, _, clock := setup(t, true)

	b := NewBar("Title", 100, 0)
	clock.advance(redrawInterval)
	outBuf.Reset()
	b.Write(make([]byte, 150))
	if got, want := outBuf.String(), barLine(35, 100, "100 B", "100 B", "00:00"); got != want {
		t.Errorf("output after overrun Write = %q, want %q", got, want)
	}
	outBuf.Reset()
	b.Finish()
	// The final frame is already on the line, so Finish only terminates it.
	if got, want := outBuf.String(), "\n"; got != want {
		t.Errorf("output after Finish = %q, want %q", got, want)
	}
}

func TestBarResumed(t *testing.T) {
	outBuf, _, _ := setup(t, true)

	if b := NewBar("Title", 100, 40); b == nil {
		t.Fatal("NewBar() = nil, want non-nil")
	}
	if got, want := outBuf.String(), "Title (100 B)\n"+barLine(14, 40, "40 B", "100 B", "00:00"); got != want {
		t.Errorf("initial output = %q, want %q", got, want)
	}
}

func TestBarUnknownTotal(t *testing.T) {
	for _, total := range []int64{0, -1} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			outBuf, _, clock := setup(t, true)

			b := NewBar("Title", total, 0)
			if b == nil {
				t.Fatal("NewBar() = nil, want non-nil")
			}
			if got, want := outBuf.String(), "Title\n\r  0 B [00:00]"; got != want {
				t.Errorf("initial output = %q, want %q", got, want)
			}

			outBuf.Reset()
			clock.advance(2 * time.Second)
			b.Write(make([]byte, 2048))
			if got, want := outBuf.String(), "\r  2.0 KiB [00:02]"; got != want {
				t.Errorf("output after Write = %q, want %q", got, want)
			}

			outBuf.Reset()
			clock.advance(time.Second)
			b.Finish()
			got := outBuf.String()
			if want := "\r  2.0 KiB [00:03]\n"; got != want {
				t.Errorf("output after Finish = %q, want %q", got, want)
			}
			if strings.Contains(got, "|") {
				t.Errorf("output after Finish = %q, want no bar cells", got)
			}
		})
	}
}

func TestBarThrottle(t *testing.T) {
	outBuf, _, clock := setup(t, true)

	b := NewBar("Title", 100, 0)
	outBuf.Reset()

	// A write immediately after the initial draw is within the interval.
	b.Write(make([]byte, 10))
	if got := strings.Count(outBuf.String(), "\r"); got != 0 {
		t.Errorf("redraws immediately after NewBar = %d, want 0", got)
	}

	// Once the interval has passed, only the first of two writes redraws.
	clock.advance(redrawInterval)
	b.Write(make([]byte, 10))
	b.Write(make([]byte, 10))
	if got := strings.Count(outBuf.String(), "\r"); got != 1 {
		t.Errorf("redraws within one interval = %d, want 1", got)
	}
	// The throttled write still counted its bytes.
	if got, want := outBuf.String(), barLine(7, 20, "20 B", "100 B", "00:00"); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	// After another interval, the accumulated total is drawn.
	outBuf.Reset()
	clock.advance(redrawInterval)
	b.Write(make([]byte, 10))
	if got, want := outBuf.String(), barLine(14, 40, "40 B", "100 B", "00:00"); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestBarAbort(t *testing.T) {
	outBuf, _, clock := setup(t, true)

	b := NewBar("Title", 100, 0)
	clock.advance(redrawInterval)
	b.Write(make([]byte, 50))
	outBuf.Reset()

	b.Abort()
	if got, want := outBuf.String(), "\n"; got != want {
		t.Errorf("output after Abort = %q, want %q", got, want)
	}
	if got := currentLastLen(); got != 0 {
		t.Errorf("lastLen after Abort = %d, want 0", got)
	}
}

func TestRedrawPadding(t *testing.T) {
	outBuf, _, _ := setup(t, true)

	mu.Lock()
	redrawLocked("abcdef")
	redrawLocked("ab")
	mu.Unlock()
	if got, want := outBuf.String(), "\rabcdef\rab    "; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if got := currentLastLen(); got != 2 {
		t.Errorf("lastLen = %d, want 2", got)
	}

	// A longer line needs no padding.
	outBuf.Reset()
	mu.Lock()
	redrawLocked("abcd")
	mu.Unlock()
	if got, want := outBuf.String(), "\rabcd"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	// Redrawing the same content writes nothing; serial console loggers
	// record every frame, so identical frames are suppressed.
	outBuf.Reset()
	mu.Lock()
	redrawLocked("abcd")
	mu.Unlock()
	if got := outBuf.String(); got != "" {
		t.Errorf("output after identical redraw = %q, want empty", got)
	}
}

func TestClearLocked(t *testing.T) {
	outBuf, _, _ := setup(t, true)

	mu.Lock()
	clearLocked()
	mu.Unlock()
	if got := outBuf.String(); got != "" {
		t.Errorf("output with nothing rendered = %q, want empty", got)
	}

	mu.Lock()
	redrawLocked("abc")
	clearLocked()
	mu.Unlock()
	if got, want := outBuf.String(), "\rabc\r   \r"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if got := currentLastLen(); got != 0 {
		t.Errorf("lastLen = %d, want 0", got)
	}
}

func TestSpinnerDone(t *testing.T) {
	outBuf, _, _ := setup(t, true)

	s := NewSpinner("Title")
	if s == nil {
		t.Fatal("NewSpinner() = nil, want non-nil")
	}
	if got := currentActive(); got != s {
		t.Errorf("active = %p, want %p", got, s)
	}
	if got, want := snapshot(outBuf), "\rTitle... - [00:00]"; !strings.HasPrefix(got, want) {
		t.Errorf("initial output = %q, want prefix %q", got, want)
	}

	s.Stop(nil)
	if got, want := outBuf.String(), "\rTitle... done [00:00]\n"; !strings.HasSuffix(got, want) {
		t.Errorf("output after Stop(nil) = %q, want suffix %q", got, want)
	}
	if got := currentActive(); got != nil {
		t.Errorf("active after Stop = %p, want nil", got)
	}
	if got := currentLastLen(); got != 0 {
		t.Errorf("lastLen after Stop = %d, want 0", got)
	}

	// Stop is idempotent.
	outBuf.Reset()
	s.Stop(nil)
	s.Stop(errors.New("x"))
	if got := outBuf.String(); got != "" {
		t.Errorf("output after repeated Stop = %q, want empty", got)
	}
}

// redirectChild points the spinner's pass-through writers at fresh buffers
// and returns them.
func redirectChild(s *Spinner) (childOut, childErr *bytes.Buffer) {
	childOut, childErr = &bytes.Buffer{}, &bytes.Buffer{}
	mu.Lock()
	defer mu.Unlock()
	s.stdout.dst = childOut
	s.stderr.dst = childErr
	return childOut, childErr
}

func TestSpinnerPassesChildOutputThrough(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantEnd string
	}{
		{name: "success", err: nil, wantEnd: "\rTitle... done [00:00]\n"},
		{name: "failure", err: errors.New("x"), wantEnd: "\rTitle... failed [00:00]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outBuf, _, _ := setup(t, true)
			s := NewSpinner("Title")
			if s == nil {
				t.Fatal("NewSpinner() = nil, want non-nil")
			}
			childOut, childErr := redirectChild(s)

			// An unterminated line is held so a spinner frame cannot split it.
			io.WriteString(Stdout(), "installing")
			if got := snapshot(childOut); got != "" {
				t.Errorf("child stdout after partial write = %q, want empty", got)
			}
			// Completing the line clears the spinner and writes it immediately.
			n := currentLastLen()
			io.WriteString(Stdout(), " step 1\nstep 2")
			if got, want := snapshot(childOut), "installing step 1\n"; got != want {
				t.Errorf("child stdout after newline = %q, want %q", got, want)
			}
			if n > 0 {
				if got, want := snapshot(outBuf), "\r"+strings.Repeat(" ", n)+"\r"; !strings.Contains(got, want) {
					t.Errorf("progress output = %q, want it to contain clear sequence %q", got, want)
				}
			}
			// Stderr stays on stderr.
			io.WriteString(Stderr(), "warning: something\n")
			if got, want := snapshot(childErr), "warning: something\n"; got != want {
				t.Errorf("child stderr = %q, want %q", got, want)
			}
			if strings.Contains(snapshot(outBuf), "step") || strings.Contains(snapshot(outBuf), "warning") {
				t.Errorf("progress output = %q, want no child output on the progress stream", snapshot(outBuf))
			}

			// Stop writes the held tail before the final status, success or not.
			s.Stop(tc.err)
			if got, want := childOut.String(), "installing step 1\nstep 2"; got != want {
				t.Errorf("child stdout after Stop = %q, want %q", got, want)
			}
			if got := outBuf.String(); !strings.HasSuffix(got, "\n"+tc.wantEnd) {
				t.Errorf("progress output after Stop = %q, want suffix %q", got, "\n"+tc.wantEnd)
			}

			// A late write from a writer handed out earlier goes straight through.
			io.WriteString(s.stdout, "late")
			if got, want := childOut.String(), "installing step 1\nstep 2late"; got != want {
				t.Errorf("child stdout after late write = %q, want %q", got, want)
			}
		})
	}
}

func TestSpinnerLongPartialLine(t *testing.T) {
	setup(t, true)
	s := NewSpinner("Title")
	if s == nil {
		t.Fatal("NewSpinner() = nil, want non-nil")
	}
	defer s.Stop(nil)
	childOut, _ := redirectChild(s)

	long := strings.Repeat("x", maxPartialLine)
	io.WriteString(Stdout(), long)
	if got := snapshot(childOut); got != long {
		t.Errorf("child stdout after %d unterminated bytes has %d bytes, want all of them", len(long), len(got))
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestSpinnerPassThroughWriteError(t *testing.T) {
	setup(t, true)
	s := NewSpinner("Title")
	if s == nil {
		t.Fatal("NewSpinner() = nil, want non-nil")
	}
	defer s.Stop(nil)
	mu.Lock()
	s.stdout.dst = errWriter{}
	mu.Unlock()

	if _, err := io.WriteString(Stdout(), "line\n"); err == nil {
		t.Error("Write() to a failing destination = nil error, want error")
	}
}

func TestSpinnerExclusive(t *testing.T) {
	setup(t, true)

	s := NewSpinner("First")
	if s == nil {
		t.Fatal("NewSpinner() = nil, want non-nil")
	}
	defer s.Stop(nil)
	if s2 := NewSpinner("Second"); s2 != nil {
		s2.Stop(nil)
		t.Errorf("NewSpinner() while another is active = %v, want nil", s2)
	}
	s.Stop(nil)
	s3 := NewSpinner("Third")
	if s3 == nil {
		t.Fatal("NewSpinner() after Stop = nil, want non-nil")
	}
	s3.Stop(nil)
}

func TestStdoutStderr(t *testing.T) {
	setup(t, true)

	if w := Stdout(); w != io.Writer(os.Stdout) {
		t.Errorf("Stdout() with no spinner = %v, want os.Stdout", w)
	}
	if w := Stderr(); w != io.Writer(os.Stderr) {
		t.Errorf("Stderr() with no spinner = %v, want os.Stderr", w)
	}

	s := NewSpinner("Title")
	if s == nil {
		t.Fatal("NewSpinner() = nil, want non-nil")
	}
	if w := Stdout(); w != io.Writer(s.stdout) {
		t.Errorf("Stdout() while active = %v, want the spinner's stdout pass-through", w)
	}
	if w := Stderr(); w != io.Writer(s.stderr) {
		t.Errorf("Stderr() while active = %v, want the spinner's stderr pass-through", w)
	}
	s.Stop(nil)

	if w := Stdout(); w != io.Writer(os.Stdout) {
		t.Errorf("Stdout() after Stop = %v, want os.Stdout", w)
	}
	if w := Stderr(); w != io.Writer(os.Stderr) {
		t.Errorf("Stderr() after Stop = %v, want os.Stderr", w)
	}
}

func TestPrintfClearsBar(t *testing.T) {
	outBuf, stdoutBuf, _ := setup(t, true)

	NewBar("Title", 100, 0)
	n := currentLastLen()
	if n == 0 {
		t.Fatal("lastLen after NewBar = 0, want > 0")
	}
	outBuf.Reset()

	Printf("hello %d\n", 1)
	if got, want := outBuf.String(), "\r"+strings.Repeat(" ", n)+"\r"; got != want {
		t.Errorf("out after Printf = %q, want %q", got, want)
	}
	if got, want := stdoutBuf.String(), "hello 1\n"; got != want {
		t.Errorf("stdout after Printf = %q, want %q", got, want)
	}
	if got := currentLastLen(); got != 0 {
		t.Errorf("lastLen after Printf = %d, want 0", got)
	}
}

func TestPrintfClearsSpinner(t *testing.T) {
	outBuf, stdoutBuf, _ := setup(t, true)

	s := NewSpinner("Title")
	if s == nil {
		t.Fatal("NewSpinner() = nil, want non-nil")
	}
	n := currentLastLen()
	if n == 0 {
		t.Fatal("lastLen after NewSpinner = 0, want > 0")
	}
	reset(outBuf)

	// The spinner goroutine may redraw before or after Printf, so only assert
	// that the clear sequence was emitted and the text went to stdout. The
	// exact clear-then-lastLen==0 behavior is checked deterministically by
	// TestPrintfClearsBar and TestClearLocked.
	Printf("hello %d\n", 2)
	got := snapshot(outBuf)
	if want := "\r" + strings.Repeat(" ", n) + "\r"; !strings.Contains(got, want) {
		t.Errorf("out after Printf = %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "hello") {
		t.Errorf("out after Printf = %q, want no console text", got)
	}
	if got, want := stdoutBuf.String(), "hello 2\n"; got != want {
		t.Errorf("stdout after Printf = %q, want %q", got, want)
	}

	s.Stop(nil)
	if got, want := outBuf.String(), "\rTitle... done [00:00]\n"; !strings.HasSuffix(got, want) {
		t.Errorf("output after Stop = %q, want suffix %q", got, want)
	}
	if got := currentLastLen(); got != 0 {
		t.Errorf("lastLen after Stop = %d, want 0", got)
	}
}

func TestElapsed(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "00:00"},
		{400 * time.Millisecond, "00:00"},
		{600 * time.Millisecond, "00:01"},
		{61 * time.Second, "01:01"},
		{-5 * time.Second, "00:00"},
		{3599 * time.Second, "59:59"},
		{3600 * time.Second, "60:00"},
	} {
		if got := elapsed(start, start.Add(tc.d)); got != tc.want {
			t.Errorf("elapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFitTitle(t *testing.T) {
	orig := termWidth
	t.Cleanup(func() { termWidth = orig })
	for _, tc := range []struct {
		width int
		title string
		want  string
	}{
		{0, "unknown width keeps the title", "unknown width keeps the title"},
		{80, "short", "short"},
		{30, "googet-package-with-a-long-name", "googet-pack"},
		{30, "ünïcödé-päckägé-nämé", "ünïcödé-päc"},
		{10, "too narrow to help", "too narrow to help"},
	} {
		termWidth = func() int { return tc.width }
		if got := fitTitle(tc.title); got != tc.want {
			t.Errorf("fitTitle(%q) at width %d = %q, want %q", tc.title, tc.width, got, tc.want)
		}
	}
}

func TestSpinnerFitsTerminalWidth(t *testing.T) {
	outBuf, _, _ := setup(t, true)
	const width = 30
	mu.Lock()
	termWidth = func() int { return width }
	mu.Unlock()

	NewSpinner("Installing googet-package-with-a-long-name").Stop(errors.New("boom"))

	got := outBuf.String()
	for _, line := range strings.FieldsFunc(got, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if len(line) >= width {
			t.Errorf("rendered line %q is %d columns, want fewer than %d", line, len(line), width)
		}
	}
	if want := "\rInstalling... failed [00:00]\n"; !strings.HasSuffix(got, want) {
		t.Errorf("output = %q, want suffix %q", got, want)
	}
}

func TestFitBarWidth(t *testing.T) {
	orig := termWidth
	t.Cleanup(func() { termWidth = orig })
	for _, tc := range []struct{ width, want int }{
		{0, barWidth},
		{200, barWidth},
		{72, barWidth},
		{60, 23},
		{47, minBarWidth},
		{46, 0},
		{20, 0},
	} {
		termWidth = func() int { return tc.width }
		if got := fitBarWidth(); got != tc.want {
			t.Errorf("fitBarWidth() at width %d = %d, want %d", tc.width, got, tc.want)
		}
	}
}

func TestBarFitsTerminalWidth(t *testing.T) {
	for _, tc := range []struct {
		width int
		want  string
	}{
		{60, "\r|=======================| 100%  1023 KiB / 1023 KiB [00:00]\n"},
		{40, "\r  100%  1023 KiB / 1023 KiB [00:00]\n"},
	} {
		t.Run(fmt.Sprint(tc.width), func(t *testing.T) {
			outBuf, _, _ := setup(t, true)
			mu.Lock()
			termWidth = func() int { return tc.width }
			mu.Unlock()

			b := NewBar("Downloading pkg", 1023<<10, 0)
			b.Write(make([]byte, 512<<10))
			b.Finish()

			got := outBuf.String()
			for _, line := range strings.FieldsFunc(got, func(r rune) bool { return r == '\r' || r == '\n' }) {
				if len(line) >= tc.width {
					t.Errorf("rendered line %q is %d columns, want fewer than %d", line, len(line), tc.width)
				}
			}
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("output = %q, want suffix %q", got, tc.want)
			}
		})
	}
}
