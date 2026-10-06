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
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"cloud.google.com/go/storage"
)

// DefaultStallTimeout is the default value of StallTimeout.
const DefaultStallTimeout = 2 * time.Minute

// StallTimeout is how long a server may take to send the response headers, a
// response body may deliver no data, or a GCS object may take to open, before
// the transfer is canceled. It is set from googet.conf.
// Downloader.StallTimeout overrides it for response bodies and GCS objects.
var StallTimeout = DefaultStallTimeout

// ErrStalled is wrapped by the read error of a transfer that received no data
// for its stall timeout.
var ErrStalled = errors.New("transfer stalled")

// stallTimeout returns d if positive, StallTimeout if positive, or
// DefaultStallTimeout.
func stallTimeout(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	if StallTimeout > 0 {
		return StallTimeout
	}
	return DefaultStallTimeout
}

// NewStallReader returns a reader of r that calls onStall, typically to cancel
// the request r belongs to, once timeout passes without a Read returning
// data. A timeout of zero or less means StallTimeout. After onStall has been
// called, the error of the failing Read wraps ErrStalled. Close, or a Read
// error, stops the timer.
func NewStallReader(r io.ReadCloser, timeout time.Duration, onStall func()) io.ReadCloser {
	timeout = stallTimeout(timeout)
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

// OpenGCS opens obj and returns its body and size. Opening the object, or a
// body that delivers no data, fails with ErrStalled after d.StallTimeout (or
// StallTimeout if zero or less). Closing body releases its context.
func (d *Downloader) OpenGCS(ctx context.Context, obj *storage.ObjectHandle) (body io.ReadCloser, size int64, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	timeout := stallTimeout(d.StallTimeout)
	stall := func() { cancel(ErrStalled) }
	// Bound NewReader explicitly because storage.Client uses its own transport
	// without Downloader's ResponseHeaderTimeout.
	t := time.AfterFunc(timeout, stall)
	r, err := obj.NewReader(ctx)
	stopped := t.Stop()
	// If t already expired (!stopped) or ctx was canceled as NewReader
	// returned, fail now rather than wrapping r with stalled == false.
	if err == nil && (!stopped || ctx.Err() != nil) {
		r.Close()
		if !stopped {
			err = cmp.Or(context.Cause(ctx), ErrStalled)
		} else {
			err = ctx.Err()
		}
	}
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
			err = fmt.Errorf("%w: %w", cause, err)
		}
		cancel(nil)
		return nil, 0, err
	}
	return cancelOnClose{NewStallReader(r, timeout, stall), cancel}, r.Attrs.Size, nil
}

// cancelOnClose cancels the context of its reader once closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel(nil)
	return c.ReadCloser.Close()
}
