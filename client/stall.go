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

package client

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ErrDownloadStalled is returned when a download connection experiences zero bytes
// read for greater than the idle timeout period.
var ErrDownloadStalled = errors.New("download stalled: no data received for timeout period")

// DefaultStallTimeout is the default duration of zero bytes received before a download is considered stalled.
const DefaultStallTimeout = 120 * time.Second

// defaultStallTimeout holds the process-wide stall timeout, in nanoseconds,
// that NewDownloader copies into Downloader.StallTimeout. Zero means
// DefaultStallTimeout.
var defaultStallTimeout atomic.Int64

// SetDefaultStallTimeout sets the stall timeout that Downloaders created by
// later NewDownloader calls use for repo index and package downloads. A value
// of zero or less restores DefaultStallTimeout. Existing Downloaders are not
// affected. It is safe for concurrent use.
//
// It deliberately mirrors supervisor.Configure: the googet command sets both
// process-wide defaults once from its settings at startup, and
// NewDownloader copies this default into Downloader.StallTimeout, which
// callers may still override per Downloader.
func SetDefaultStallTimeout(d time.Duration) {
	if d < 0 {
		d = 0
	}
	defaultStallTimeout.Store(int64(d))
}

// currentDefaultStallTimeout returns the timeout set by SetDefaultStallTimeout,
// or DefaultStallTimeout if none is set.
func currentDefaultStallTimeout() time.Duration {
	if d := time.Duration(defaultStallTimeout.Load()); d > 0 {
		return d
	}
	return DefaultStallTimeout
}

// StallReader wraps an io.Reader (typically an http.Response.Body) with an idle-read watchdog timer.
type StallReader struct {
	r       io.Reader
	timeout time.Duration
	cancel  context.CancelFunc

	mu      sync.Mutex
	timer   *time.Timer
	stalled bool
	closed  bool
	// canceled is closed once onStall has invoked cancel, so that a Read
	// returning ErrDownloadStalled guarantees the context is already done.
	canceled chan struct{}
}

// NewStallReader returns a StallReader that wraps r with an idle read watchdog.
// When no bytes are read for timeout, cancel is invoked so that any blocked
// read on r (which must honor the canceled context) returns, and subsequent
// reads return ErrDownloadStalled. A timeout of zero or less uses
// DefaultStallTimeout. Callers must call Close to release the timer.
func NewStallReader(r io.Reader, timeout time.Duration, cancel context.CancelFunc) *StallReader {
	if timeout <= 0 {
		timeout = DefaultStallTimeout
	}
	s := &StallReader{
		r:        r,
		timeout:  timeout,
		cancel:   cancel,
		canceled: make(chan struct{}),
	}
	s.timer = time.AfterFunc(timeout, s.onStall)
	return s
}

// onStall is invoked by the idle timer when the timeout expires without reading bytes.
func (s *StallReader) onStall() {
	s.mu.Lock()
	if s.closed || s.stalled {
		s.mu.Unlock()
		return
	}
	// stalled is set before cancel so that a read unblocked by the
	// cancellation reports ErrDownloadStalled rather than context.Canceled.
	s.stalled = true
	s.mu.Unlock()
	defer close(s.canceled)

	// Cancel context outside the mutex to prevent deadlocks.
	if s.cancel != nil {
		s.cancel()
	}
}

// stallErr waits for onStall to finish canceling and returns
// ErrDownloadStalled. It must be called without holding s.mu.
func (s *StallReader) stallErr() error {
	<-s.canceled
	return ErrDownloadStalled
}

// isStalled reports whether the idle timer has fired.
func (s *StallReader) isStalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stalled
}

// Read reads from the underlying reader. Any read that returns n > 0 bytes resets
// the idle timer. If the idle timer expires before bytes arrive, it returns ErrDownloadStalled.
func (s *StallReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.stalled {
		s.mu.Unlock()
		return 0, s.stallErr()
	}
	if s.closed {
		s.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	s.mu.Unlock()

	n, err := s.r.Read(p)

	s.mu.Lock()
	if s.stalled {
		s.mu.Unlock()
		// If the stream finished cleanly with io.EOF, prefer EOF over stall.
		if err == io.EOF {
			return n, io.EOF
		}
		return 0, s.stallErr()
	}
	defer s.mu.Unlock()

	if n > 0 {
		// Forward progress made: reset the idle timer.
		if !s.closed && s.timer != nil {
			s.timer.Reset(s.timeout)
		}
	}

	if err != nil {
		// Stop the timer on stream termination (EOF or non-stall network error).
		if s.timer != nil {
			s.timer.Stop()
		}
	}

	return n, err
}

// Close stops the idle timer and closes the underlying reader if it implements io.Closer.
func (s *StallReader) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()

	if c, ok := s.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
