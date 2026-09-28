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

// Package progress renders a download progress bar and an install spinner on
// interactive terminals.
//
// Rendering is opt-in: nothing is written unless Init has enabled it and
// stderr is a terminal. Every constructor returns nil when rendering is
// disabled and every method is nil-safe, so callers never need to branch on
// whether progress is active.
//
// Child process output is never hidden. While a spinner is active, installer
// output is passed through to the real stdout and stderr a complete line at a
// time, after first clearing the spinner line, so it scrolls above the
// spinner instead of being interleaved with it.
package progress

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"golang.org/x/term"
)

const (
	// barWidth is the number of cells in the rendered progress bar.
	barWidth = 35
	// redrawInterval throttles how often the download bar is redrawn. Serial
	// console loggers record every frame, so this is deliberately coarser
	// than what an interactive terminal alone would need.
	redrawInterval = 200 * time.Millisecond
	// spinInterval is how often the spinner advances a frame.
	spinInterval = 250 * time.Millisecond
	// maxPartialLine bounds how much of an unterminated child output line is
	// held back before it is written out anyway.
	maxPartialLine = 4096
)

// frames are the ASCII spinner frames; ASCII keeps rendering identical on
// conhost, Windows Terminal, SSH sessions and serial consoles.
var frames = []byte{'-', '\\', '|', '/'}

var (
	// mu guards all package state and serializes writes to out.
	mu sync.Mutex
	// enabled reports whether progress output is rendered.
	enabled bool
	// out is where progress lines are rendered; stderr, like curl and Cabbie.
	out io.Writer = os.Stderr
	// stdout is where non-progress console text is written.
	stdout io.Writer = os.Stdout
	// active is the spinner currently owning the console line, if any.
	active *Spinner
	// lastLine is the line currently rendered with a carriage return, used to
	// blank stale characters when a shorter line replaces it and to skip
	// redraws that would not change anything (serial console loggers record
	// every frame, so identical frames are pure noise).
	lastLine string
	// now is overridable by tests.
	now = time.Now
)

// Init enables progress rendering when allow is true, stderr is a terminal
// and the terminal is not declared dumb. It should be called once from main
// after flag parsing; until then rendering is disabled.
func Init(allow bool) {
	mu.Lock()
	defer mu.Unlock()
	// TERM=dumb (Emacs shells, some CI and serial consoles) means carriage
	// returns are not interpreted, so a redrawn line would render as garbage.
	enabled = allow && os.Getenv("TERM") != "dumb" && isTerminal(os.Stderr)
}

// isTerminal reports whether f is an interactive terminal or Windows console.
// It is a variable so tests can stub it.
var isTerminal = func(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// termWidth returns the width of the stderr terminal in columns, or 0 if it
// is unknown. It is a variable so tests can stub it.
var termWidth = func() int {
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil {
		return 0
	}
	return w
}

// spinnerSuffixLen is the widest text a spinner renders after its title.
const spinnerSuffixLen = len("... failed [00:00]")

// fitTitle cuts title so a spinner line fits within the terminal width,
// keeping the last column free. A line that wraps cannot be redrawn with a
// carriage return, so each frame would otherwise land on a new line.
func fitTitle(title string) string {
	room := termWidth() - 1 - spinnerSuffixLen
	if r := []rune(title); room > 0 && len(r) > room {
		return strings.TrimRight(string(r[:room]), " ")
	}
	return title
}

// Enabled reports whether progress output is being rendered.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled
}

// Stdout returns the writer that child processes should use for console
// stdout. With no active spinner it is os.Stdout itself. While a spinner is
// active it is a pass-through to os.Stdout that clears the spinner line before
// writing each batch of complete lines; nothing is withheld or discarded.
func Stdout() io.Writer {
	mu.Lock()
	defer mu.Unlock()
	if active != nil {
		return active.stdout
	}
	return os.Stdout
}

// Stderr returns the writer that child processes should use for console
// stderr. It behaves like Stdout, targeting os.Stderr.
func Stderr() io.Writer {
	mu.Lock()
	defer mu.Unlock()
	if active != nil {
		return active.stderr
	}
	return os.Stderr
}

// Printf writes to stdout like fmt.Printf, first clearing any active bar or
// spinner line so the text starts at column zero. The bar or spinner redraws
// itself on its next update.
func Printf(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	clearLocked()
	fmt.Fprintf(stdout, format, a...)
}

// redrawLocked overwrites the current console line with s, blanking any
// trailing characters left over from a longer previous line. It writes
// nothing if s is already what is on the line.
func redrawLocked(s string) {
	if s == lastLine {
		return
	}
	pad := ""
	if n := len(lastLine) - len(s); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprintf(out, "\r%s%s", s, pad)
	lastLine = s
}

// clearLocked blanks the current console line, if one was rendered.
func clearLocked() {
	if lastLine == "" {
		return
	}
	fmt.Fprintf(out, "\r%s\r", strings.Repeat(" ", len(lastLine)))
	lastLine = ""
}

// endLineLocked terminates the current console line.
func endLineLocked() {
	fmt.Fprintln(out)
	lastLine = ""
}

// elapsed formats the time since start as mm:ss.
func elapsed(start, t time.Time) string {
	d := t.Sub(start).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%02d:%02d", int(d/time.Minute), int((d%time.Minute)/time.Second))
}

// Bar is a byte-counting progress bar that implements io.Writer so it can be
// added to an io.MultiWriter alongside the real destination.
type Bar struct {
	title    string
	total    int64 // A total <= 0 means the size is unknown.
	cur      int64
	start    time.Time
	lastDraw time.Time
}

// NewBar prints a title line and returns a bar expecting total bytes, of
// which initial bytes are already complete (for resumed downloads). It returns
// nil when rendering is disabled.
func NewBar(title string, total, initial int64) *Bar {
	mu.Lock()
	defer mu.Unlock()
	if !enabled {
		return nil
	}
	clearLocked()
	if total > 0 {
		fmt.Fprintf(out, "%s (%s)\n", title, humanize.IBytes(uint64(total)))
	} else {
		fmt.Fprintf(out, "%s\n", title)
	}
	b := &Bar{title: title, total: total, cur: initial, start: now()}
	b.drawLocked(b.start)
	return b
}

// Write records len(p) bytes of progress and redraws the bar at most once per
// redrawInterval. It never fails, so it cannot abort the surrounding copy.
func (b *Bar) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	mu.Lock()
	defer mu.Unlock()
	b.cur += int64(len(p))
	if t := now(); t.Sub(b.lastDraw) >= redrawInterval {
		b.drawLocked(t)
	}
	return len(p), nil
}

// Finish renders the bar as complete and terminates the line.
func (b *Bar) Finish() {
	if b == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if b.total > 0 {
		b.cur = b.total
	}
	b.drawLocked(now())
	endLineLocked()
}

// Abort terminates the line without marking the bar complete, leaving the
// partial state visible above whatever error follows.
func (b *Bar) Abort() {
	if b == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	endLineLocked()
}

// drawLocked renders the bar line for time t.
func (b *Bar) drawLocked(t time.Time) {
	b.lastDraw = t
	if b.total <= 0 {
		redrawLocked(fmt.Sprintf("  %s [%s]", humanize.IBytes(uint64(b.cur)), elapsed(b.start, t)))
		return
	}
	cur := b.cur
	if cur > b.total {
		cur = b.total
	}
	pct := int(cur * 100 / b.total)
	bar := "  "
	if w := fitBarWidth(); w > 0 {
		n := pct * w / 100
		bar = "|" + strings.Repeat("=", n) + strings.Repeat("-", w-n) + "| "
	}
	redrawLocked(fmt.Sprintf("%s%3d%%  %s / %s [%s]", bar, pct,
		humanize.IBytes(uint64(cur)), humanize.IBytes(uint64(b.total)), elapsed(b.start, t)))
}

// barTextLen is the widest text a bar line renders besides its cells.
const barTextLen = len("|| 100%  1023 KiB / 1023 KiB [00:00]")

// minBarWidth is the narrowest bar worth drawing; below it only the
// percentage and byte counts are shown.
const minBarWidth = 10

// fitBarWidth returns how many bar cells fit within the terminal width,
// keeping the last column free, or 0 if the bar should be omitted. An unknown
// width gets the full barWidth.
func fitBarWidth() int {
	w := termWidth()
	if w <= 0 {
		return barWidth
	}
	n := min(barWidth, w-1-barTextLen)
	if n < minBarWidth {
		return 0
	}
	return n
}

// Spinner renders an indeterminate "title... /" line until Stop is called.
// At most one spinner is active at a time.
type Spinner struct {
	title   string
	start   time.Time
	done    chan struct{}
	wg      sync.WaitGroup
	stopped bool
	// stdout and stderr pass child process output through to the real
	// streams while the spinner is active.
	stdout *lineWriter
	stderr *lineWriter
}

// NewSpinner starts rendering a spinner for title. It returns nil when
// rendering is disabled or another spinner is already active.
func NewSpinner(title string) *Spinner {
	mu.Lock()
	defer mu.Unlock()
	if !enabled || active != nil {
		return nil
	}
	clearLocked()
	s := &Spinner{title: title, start: now(), done: make(chan struct{})}
	s.stdout = &lineWriter{owner: s, dst: os.Stdout}
	s.stderr = &lineWriter{owner: s, dst: os.Stderr}
	active = s
	s.drawLocked(0, s.start)
	s.wg.Add(1)
	go s.run()
	return s
}

// run redraws the spinner until done is closed.
func (s *Spinner) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(spinInterval)
	defer ticker.Stop()
	for i := 1; ; i++ {
		select {
		case <-s.done:
			return
		case t := <-ticker.C:
			mu.Lock()
			s.drawLocked(i, t)
			mu.Unlock()
		}
	}
}

// drawLocked renders spinner frame i for time t.
func (s *Spinner) drawLocked(i int, t time.Time) {
	redrawLocked(fmt.Sprintf("%s... %c [%s]", fitTitle(s.title), frames[i%len(frames)], elapsed(s.start, t)))
}

// Stop ends the spinner, writing out any unterminated child output line and
// then rendering "done" or, when err is non-nil, "failed". Child output has
// already been shown as it was produced, so nothing else is printed. Stop is
// idempotent.
func (s *Spinner) Stop(err error) {
	if s == nil {
		return
	}
	mu.Lock()
	if s.stopped {
		mu.Unlock()
		return
	}
	s.stopped = true
	mu.Unlock()

	close(s.done)
	s.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// A console write error here has nowhere better to be reported.
	_ = s.stdout.flushLocked()
	_ = s.stderr.flushLocked()
	status := "done"
	if err != nil {
		status = "failed"
	}
	redrawLocked(fmt.Sprintf("%s... %s [%s]", fitTitle(s.title), status, elapsed(s.start, now())))
	endLineLocked()
	active = nil
}

// lineWriter passes child process output through to dst while its owning
// spinner is active. Complete lines are written immediately after clearing
// the spinner line; an unterminated tail is held until its newline arrives,
// it grows past maxPartialLine, or the spinner stops, so a spinner redraw
// never lands in the middle of an installer's line. Once the owner is no
// longer active, writes go straight to dst.
type lineWriter struct {
	owner   *Spinner
	dst     io.Writer
	pending []byte
}

// Write passes p through to dst. It reports an error only if writing to dst
// fails, matching the behavior of writing to dst directly.
func (w *lineWriter) Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	if active != w.owner {
		if err := w.flushLocked(); err != nil {
			return 0, err
		}
		return w.dst.Write(p)
	}
	w.pending = append(w.pending, p...)
	if i := bytes.LastIndexByte(w.pending, '\n'); i >= 0 {
		clearLocked()
		if _, err := w.dst.Write(w.pending[:i+1]); err != nil {
			w.pending = nil
			return 0, err
		}
		w.pending = append(w.pending[:0], w.pending[i+1:]...)
	}
	if len(w.pending) >= maxPartialLine {
		if err := w.flushLocked(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flushLocked writes out any held unterminated line. It then ends the
// console line on the progress stream so the next spinner frame does not
// overwrite the partial text; the child's own stream is left byte-exact.
func (w *lineWriter) flushLocked() error {
	if len(w.pending) == 0 {
		return nil
	}
	clearLocked()
	_, err := w.dst.Write(w.pending)
	w.pending = nil
	endLineLocked()
	return err
}
