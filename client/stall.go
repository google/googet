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
	"fmt"
	"io"
	"time"
)

// defaultStallTimeout is how long a transfer may receive no data before it
// is canceled as stalled.
const defaultStallTimeout = 2 * time.Minute

// ErrStalled is the cancellation cause of a transfer that received no data
// for its stall timeout.
var ErrStalled = errors.New("transfer stalled")

// StallWatch cancels a transfer's context once the transfer goes a timeout
// without receiving data. A transfer that keeps receiving data is never
// canceled.
type StallWatch struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	timer   *time.Timer
	timeout time.Duration
}

// WatchStall returns a context derived from ctx that is canceled with cause
// ErrStalled once timeout passes without progress, and the StallWatch that
// tracks progress. The timer starts immediately, so a request that never gets
// a response is covered too. A timeout of zero or less means two minutes.
// Callers must call Stop.
func WatchStall(ctx context.Context, timeout time.Duration) (context.Context, *StallWatch) {
	if timeout <= 0 {
		timeout = defaultStallTimeout
	}
	ctx, cancel := context.WithCancelCause(ctx)
	w := &StallWatch{ctx: ctx, cancel: cancel, timeout: timeout}
	w.timer = time.AfterFunc(timeout, func() {
		cancel(fmt.Errorf("%w: no data received for %v", ErrStalled, timeout))
	})
	return ctx, w
}

// Progress restarts the stall timer, for example when response headers
// arrive.
func (w *StallWatch) Progress() {
	w.timer.Reset(w.timeout)
}

// Reader wraps r so that every Read returning data restarts the stall timer.
func (w *StallWatch) Reader(r io.ReadCloser) io.ReadCloser {
	return &stallReader{ReadCloser: r, w: w}
}

// Stop releases the timer. If *errp is non-nil and the transfer stalled, it
// also wraps *errp with the stall cause so that callers can detect
// ErrStalled even when the failing library only reports context.Canceled.
func (w *StallWatch) Stop(errp *error) {
	w.timer.Stop()
	if cause := context.Cause(w.ctx); *errp != nil && !errors.Is(*errp, ErrStalled) && errors.Is(cause, ErrStalled) {
		*errp = fmt.Errorf("%w: %w", cause, *errp)
	}
	w.cancel(nil)
}

// stallReader restarts its StallWatch's timer whenever a Read returns data.
type stallReader struct {
	io.ReadCloser
	w *StallWatch
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if n > 0 {
		s.w.Progress()
	}
	return n, err
}
