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
	"cmp"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// DefaultStallTimeout is how long a response body may deliver no data, or a
// GCS object take to open, unless Downloader.StallTimeout says otherwise. It
// is also the response header timeout of NewDownloader.
const DefaultStallTimeout = 2 * time.Minute

// ErrStalled is wrapped by the read error of a transfer that received no data
// for its stall timeout.
var ErrStalled = errors.New("transfer stalled")

// NewStallReader returns a reader of r that calls onStall, typically to cancel
// the request r belongs to, once timeout passes without a Read returning
// data. A timeout of zero means DefaultStallTimeout. After onStall has been
// called, the error of the failing Read wraps ErrStalled. Close, or a Read
// error, stops the timer.
func NewStallReader(r io.ReadCloser, timeout time.Duration, onStall func()) io.ReadCloser {
	timeout = cmp.Or(timeout, DefaultStallTimeout)
	s := &stallReader{ReadCloser: r, timeout: timeout}
	s.timer = time.AfterFunc(timeout, func() {
		s.stalled.Store(true)
		onStall()
	})
	return s
}

// stallReader restarts its timer whenever a Read returns data.
type stallReader struct {
	io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	stalled atomic.Bool
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	switch {
	case err != nil:
		s.timer.Stop()
		// A transfer that reached EOF ended rather than stalled. net/http
		// already returns ErrStalled, the cause the context was canceled
		// with.
		if s.stalled.Load() && err != io.EOF && !errors.Is(err, ErrStalled) {
			err = fmt.Errorf("%w: no data received for %v: %w", ErrStalled, s.timeout, err)
		}
	case n > 0:
		s.timer.Reset(s.timeout)
	}
	return n, err
}

func (s *stallReader) Close() error {
	s.timer.Stop()
	return s.ReadCloser.Close()
}
