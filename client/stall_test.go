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
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// The timing constants below keep wide margins so that the tests stay
// reliable on loaded machines and under the race detector. Tests that expect
// no stall deliver data far more often than the stall timeout, and tests that
// expect a stall wait on the cancel callback with a deadline far longer than
// the stall timeout instead of sleeping for a fixed time.
const (
	// trickleTimeout is the stall timeout for streams that must not stall.
	trickleTimeout = 250 * time.Millisecond
	// trickleDelay is the delay between chunks of a stream that must not
	// stall. It is 50 times shorter than trickleTimeout.
	trickleDelay = 5 * time.Millisecond
	// stallTimeout is the stall timeout for streams that must stall.
	stallTimeout = 30 * time.Millisecond
	// stallDeadline bounds how long a test waits for an expected stall.
	stallDeadline = 10 * time.Second
)

// cancelRecorder records calls to its cancel method.
type cancelRecorder struct {
	once sync.Once
	done chan struct{}
}

func newCancelRecorder() *cancelRecorder {
	return &cancelRecorder{done: make(chan struct{})}
}

// cancel records that the stall callback ran.
func (c *cancelRecorder) cancel() {
	c.once.Do(func() { close(c.done) })
}

// canceled reports whether cancel has been called.
func (c *cancelRecorder) canceled() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// wait reports whether cancel is called within stallDeadline.
func (c *cancelRecorder) wait() bool {
	select {
	case <-c.done:
		return true
	case <-time.After(stallDeadline):
		return false
	}
}

func TestStallReader_Normal(t *testing.T) {
	// Verify that reading a complete buffer through StallReader succeeds without error.
	data := []byte("hello world from googet stall reader test")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	sr := NewStallReader(bytes.NewReader(data), stallDeadline, cancel)
	defer sr.Close()

	buf, err := io.ReadAll(sr)
	if err != nil {
		t.Fatalf("unexpected error reading from StallReader: %v", err)
	}
	if !bytes.Equal(buf, data) {
		t.Errorf("read content mismatch: got %q, want %q", string(buf), string(data))
	}
}

type trickleReader struct {
	chunks [][]byte
	delay  time.Duration
	index  int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

func TestStallReader_SlowTrickle(t *testing.T) {
	// Verify that slow trickle streams complete as long as chunks arrive within timeout.
	chunks := [][]byte{
		[]byte("chunk1-"),
		[]byte("chunk2-"),
		[]byte("chunk3-"),
		[]byte("chunk4-"),
		[]byte("chunk5"),
	}
	r := &trickleReader{chunks: chunks, delay: trickleDelay}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	sr := NewStallReader(r, trickleTimeout, cancel)
	defer sr.Close()

	buf, err := io.ReadAll(sr)
	if err != nil {
		t.Fatalf("slow trickle read unexpectedly failed: %v", err)
	}
	expected := "chunk1-chunk2-chunk3-chunk4-chunk5"
	if string(buf) != expected {
		t.Errorf("trickle content mismatch: got %q, want %q", string(buf), expected)
	}
}

func TestStallReader_StallDetection(t *testing.T) {
	// Verify that StallReader returns ErrDownloadStalled when data transfer freezes.
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	_, cancel := context.WithCancel(context.Background())
	// Cancel wrapper closes pipe to unblock Read.
	cancelWrapper := func() {
		cancel()
		pw.CloseWithError(context.Canceled)
	}

	sr := NewStallReader(pr, stallTimeout, cancelWrapper)
	defer sr.Close()

	// Write initial chunk, then stall.
	go func() {
		pw.Write([]byte("initial data"))
		// Do not write anything further.
	}()

	buf := make([]byte, 1024)
	n, err := sr.Read(buf)
	if err != nil {
		t.Fatalf("first read failed: %v", err)
	}
	if n != len("initial data") {
		t.Fatalf("got %d bytes on first read, want %d", n, len("initial data"))
	}

	// The second read blocks until the stall timer cancels it.
	n, err = sr.Read(buf)
	if !errors.Is(err, ErrDownloadStalled) {
		t.Fatalf("expected ErrDownloadStalled on stalled stream, got: %v (n=%d)", err, n)
	}
}

func TestStallReader_ExternalCancellation(t *testing.T) {
	// Verify that external context cancellation is preserved and not reported as a stall.
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sr := NewStallReader(pr, stallDeadline, cancel)
	defer sr.Close()

	// Cancel external context well before stall timeout.
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
		pw.CloseWithError(ctx.Err())
	}()

	buf := make([]byte, 64)
	_, err := sr.Read(buf)
	if errors.Is(err, ErrDownloadStalled) {
		t.Fatalf("misreported external context cancellation as ErrDownloadStalled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

type zeroByteReader struct {
	delay time.Duration
	mu    sync.Mutex
	calls int
}

func (z *zeroByteReader) Read(p []byte) (int, error) {
	z.mu.Lock()
	z.calls++
	z.mu.Unlock()
	if z.delay > 0 {
		time.Sleep(z.delay)
	}
	return 0, nil
}

func (z *zeroByteReader) callCount() int {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.calls
}

func TestStallReader_ZeroByteReadsDoNotResetTimer(t *testing.T) {
	// Verify that repeated zero-byte reads (n=0, err=nil) do NOT reset the idle timer.
	z := &zeroByteReader{delay: time.Millisecond}
	c := newCancelRecorder()

	// start is taken before the timer is created, so the stall can never be
	// observed earlier than stallTimeout after it.
	start := time.Now()
	sr := NewStallReader(z, stallTimeout, c.cancel)
	defer sr.Close()

	buf := make([]byte, 64)
	var stallErr error
	var readCount int

	for time.Since(start) < stallDeadline {
		readCount++
		n, err := sr.Read(buf)
		if n != 0 {
			t.Fatalf("expected 0 bytes, got %d", n)
		}
		if err != nil {
			stallErr = err
			break
		}
	}

	elapsed := time.Since(start)
	if !errors.Is(stallErr, ErrDownloadStalled) {
		t.Fatalf("expected ErrDownloadStalled from repeated zero-byte reads, got: %v (elapsed: %v, reads: %d)", stallErr, elapsed, readCount)
	}

	if elapsed < stallTimeout {
		t.Errorf("stalled prematurely: elapsed %v < timeout %v", elapsed, stallTimeout)
	}

	if !c.canceled() {
		t.Error("expected cancel() to be invoked on stall, but it was not")
	}

	// Verify subsequent reads immediately return ErrDownloadStalled without calling underlying reader.
	callsBefore := z.callCount()
	n, err := sr.Read(buf)
	if !errors.Is(err, ErrDownloadStalled) || n != 0 {
		t.Errorf("subsequent read got (%d, %v), want (0, ErrDownloadStalled)", n, err)
	}
	if got := z.callCount(); got != callsBefore {
		t.Errorf("underlying reader called after stall: %d -> %d", callsBefore, got)
	}
}

func TestStallReader_EmptyBufferRead(t *testing.T) {
	// Verify that Read with empty buffer (len=0) does NOT prevent the idle
	// timer from expiring.
	data := []byte("some data")
	r := bytes.NewReader(data)
	c := newCancelRecorder()
	sr := NewStallReader(r, stallTimeout, c.cancel)
	defer sr.Close()

	// Read with an empty buffer.
	n, err := sr.Read([]byte{})
	if n != 0 || err != nil {
		t.Fatalf("Read([]byte{}) got (%d, %v), want (0, nil)", n, err)
	}

	// Since n was 0, the timer must still expire.
	if !c.wait() {
		t.Fatalf("cancel was not called within %v after an empty buffer read with stall timeout %v", stallDeadline, stallTimeout)
	}

	buf := make([]byte, 10)
	n, err = sr.Read(buf)
	if !errors.Is(err, ErrDownloadStalled) {
		t.Fatalf("expected ErrDownloadStalled after timeout, got: (%d, %v)", n, err)
	}
}

func TestStallReader_ExtendedSlowTrickle(t *testing.T) {
	// Verify that a slow trickle stream whose total duration is several times
	// the stall timeout completes successfully without premature abortion.
	numChunks := 150
	chunkSize := 10
	var chunks [][]byte
	var expected bytes.Buffer
	for i := 0; i < numChunks; i++ {
		chunk := bytes.Repeat([]byte{byte('a' + (i % 26))}, chunkSize)
		chunks = append(chunks, chunk)
		expected.Write(chunk)
	}

	r := &trickleReader{chunks: chunks, delay: trickleDelay}
	c := newCancelRecorder()
	sr := NewStallReader(r, trickleTimeout, c.cancel)
	defer sr.Close()

	start := time.Now()
	buf, err := io.ReadAll(sr)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("extended slow trickle failed prematurely: %v (elapsed: %v)", err, elapsed)
	}

	if !bytes.Equal(buf, expected.Bytes()) {
		t.Errorf("read content mismatch: got %d bytes, want %d bytes", len(buf), expected.Len())
	}

	// Total elapsed is at least 150 * 5ms = 750ms, which is 3x the timeout.
	minExpectedElapsed := time.Duration(numChunks) * trickleDelay
	if elapsed < minExpectedElapsed {
		t.Errorf("elapsed time %v < expected %v", elapsed, minExpectedElapsed)
	}

	if c.canceled() {
		t.Error("cancel() was unexpectedly invoked on successful slow trickle stream")
	}
}

func TestStallReader_CompleteStallReliability(t *testing.T) {
	// Verify that complete stalls reliably trigger ErrDownloadStalled and context cancellation across multiple runs.
	for i := 0; i < 5; i++ {
		pr, pw := io.Pipe()
		c := newCancelRecorder()
		cancel := func() {
			c.cancel()
			pw.CloseWithError(context.Canceled)
		}

		// start is taken before the first read resets the timer, so the stall
		// can never be observed earlier than stallTimeout after it.
		start := time.Now()
		sr := NewStallReader(pr, stallTimeout, cancel)

		// Write initial byte then stall completely.
		go func() {
			pw.Write([]byte("x"))
		}()

		buf := make([]byte, 10)
		n, err := sr.Read(buf)
		if err != nil || n != 1 {
			sr.Close()
			t.Fatalf("iter %d: initial read failed: n=%d, err=%v", i, n, err)
		}

		_, err = sr.Read(buf)
		elapsed := time.Since(start)
		sr.Close()

		if !errors.Is(err, ErrDownloadStalled) {
			t.Fatalf("iter %d: expected ErrDownloadStalled, got %v", i, err)
		}
		if elapsed < stallTimeout {
			t.Errorf("iter %d: stall triggered prematurely: %v < %v", i, elapsed, stallTimeout)
		}
		if !c.canceled() {
			t.Fatalf("iter %d: context cancel func was not called on stall", i)
		}
	}
}

func TestStallReader_ConcurrentStress(t *testing.T) {
	// Concurrently run multiple StallReaders with mixed behaviors (trickle, stall, zero-byte).
	var wg sync.WaitGroup
	numWorkers := 20

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			if workerID%3 == 0 {
				// Trickle worker.
				chunks := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
				r := &trickleReader{chunks: chunks, delay: trickleDelay}
				sr := NewStallReader(r, trickleTimeout, nil)
				defer sr.Close()
				data, err := io.ReadAll(sr)
				if err != nil || string(data) != "abc" {
					t.Errorf("worker %d trickle failed: %v, got %q", workerID, err, string(data))
				}
			} else if workerID%3 == 1 {
				// Stall worker.
				pr, pw := io.Pipe()
				sr := NewStallReader(pr, stallTimeout, func() {
					pw.CloseWithError(context.Canceled)
				})
				defer sr.Close()
				buf := make([]byte, 10)
				_, err := sr.Read(buf)
				if !errors.Is(err, ErrDownloadStalled) {
					t.Errorf("worker %d expected stall, got %v", workerID, err)
				}
			} else {
				// Zero-byte reader worker.
				z := &zeroByteReader{delay: time.Millisecond}
				sr := NewStallReader(z, stallTimeout, nil)
				defer sr.Close()
				buf := make([]byte, 10)
				for {
					_, err := sr.Read(buf)
					if err != nil {
						if !errors.Is(err, ErrDownloadStalled) {
							t.Errorf("worker %d expected stall, got %v", workerID, err)
						}
						break
					}
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestStallReader_CloseStopsTimer(t *testing.T) {
	// Verify that Close stops the idle timer so no cancel fires afterwards.
	c := newCancelRecorder()
	sr := NewStallReader(bytes.NewReader([]byte("data")), trickleTimeout, c.cancel)
	if err := sr.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	// Wait well past the timeout that Close must have stopped.
	time.Sleep(3 * trickleTimeout)
	if c.canceled() {
		t.Error("cancel was invoked after Close")
	}
	if sr.isStalled() {
		t.Error("isStalled() = true after Close, want false")
	}
}

func TestSetDefaultStallTimeout(t *testing.T) {
	// Verify that NewDownloader copies the process-wide default stall timeout
	// and that zero or negative values restore DefaultStallTimeout.
	t.Cleanup(func() { SetDefaultStallTimeout(0) })
	tests := []struct {
		set  time.Duration
		want time.Duration
	}{
		{0, DefaultStallTimeout},
		{5 * time.Second, 5 * time.Second},
		{-1, DefaultStallTimeout},
	}
	for _, tc := range tests {
		SetDefaultStallTimeout(tc.set)
		d, err := NewDownloader("")
		if err != nil {
			t.Fatalf("NewDownloader: %v", err)
		}
		if d.StallTimeout != tc.want {
			t.Errorf("NewDownloader().StallTimeout after SetDefaultStallTimeout(%v) = %v, want %v", tc.set, d.StallTimeout, tc.want)
		}
	}
}
