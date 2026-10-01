/*
Copyright 2016 Google Inc. All Rights Reserved.
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

package download

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
	"google.golang.org/api/googleapi"
)

// realSleep is the production sleep implementation, captured before tests
// replace it with a fast fake.
var realSleep = sleep

// sleepRecorder records requested backoff delays without sleeping.
type sleepRecorder struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (r *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	r.mu.Unlock()
	return ctx.Err()
}

func (r *sleepRecorder) get() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.delays...)
}

func init() {
	logger.Init("test", true, false, io.Discard)
	// Tests must not wait for real backoff delays.
	sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
}

// recordSleepsForTest replaces sleep with a recorder for the duration of t.
func recordSleepsForTest(t *testing.T) *sleepRecorder {
	t.Helper()
	r := &sleepRecorder{}
	orig := sleep
	sleep = r.sleep
	t.Cleanup(func() { sleep = orig })
	return r
}

func TestExtractPkg(t *testing.T) {
	t.Parallel()
	tempDir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)
	tempFile := filepath.Join(tempDir, "test.pkg")
	f, err := oswrap.Create(tempFile)
	if err != nil {
		t.Fatalf("error creating temp file: %v", err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	name := "foo/../test"
	body := "this is a test file"
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0600,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("error writing file: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("error closing tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("error closing gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error closing file: %v", err)
	}

	dst, err := ExtractPkg(tempFile)
	if err != nil {
		t.Fatalf("error running ExtractPkg: %v", err)
	}

	cts, err := os.ReadFile(filepath.Join(dst, filepath.Clean(name)))
	if err != nil {
		t.Fatalf("error opening test file: %v", err)
	}
	if string(cts) != body {
		t.Errorf("contents of extracted file does not match expected contents: got: %q, want: %q", string(cts), body)
	}
}

func TestExtractPkgPathTraversal(t *testing.T) {
	t.Parallel()
	tempDir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)
	tempFile := filepath.Join(tempDir, "test.pkg")
	f, err := oswrap.Create(tempFile)
	if err != nil {
		t.Fatalf("error creating temp file: %v", err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	name := "foo/../../test"
	body := "this is a test file"
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0600,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("error writing file: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("error closing tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("error closing gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error closing file: %v", err)
	}

	if _, err := ExtractPkg(tempFile); err == nil {
		t.Fatal("error expected because of path traversal")
	}
}

func TestPackageHTTP_ExtendedTrickle(t *testing.T) {
	// Verify that packageHTTP completes a download stream where chunks trickle continuously
	// over a duration significantly exceeding the stallTimeout.

	numChunks := 15
	var payload []byte
	for i := 0; i < numChunks; i++ {
		payload = append(payload, []byte(fmt.Sprintf("chunk-%02d;", i))...)
	}
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		chunkSize := len(payload) / numChunks
		for i := 0; i < len(payload); i += chunkSize {
			end := i + chunkSize
			if end > len(payload) {
				end = len(payload)
			}
			w.Write(payload[i:end])
			if ok {
				flusher.Flush()
			}
			time.Sleep(15 * time.Millisecond) // 15ms is less than the 30ms timeout.
		}
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 30 * time.Millisecond

	dst := filepath.Join(t.TempDir(), "extended_trickle.pkg")
	start := time.Now()
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed for extended trickle: %v", err)
	}
	elapsed := time.Since(start)

	// Total duration is at least 15 * 15ms = 225ms, which is > 7x the 30ms stallTimeout.
	if elapsed < 200*time.Millisecond {
		t.Errorf("elapsed time %v < expected 200ms", elapsed)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(payload))
	}
}

func TestPackageHTTP_Normal(t *testing.T) {
	t.Parallel()
	// Verify that a standard HTTP download completes successfully.
	payload := []byte("standard package download payload content")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "normal.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed for normal download: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(payload))
	}
}

// TestPackageHTTP_ResumeFromDisk ports upstream's TestPackageHTTP table. It
// checks which GET requests are sent for a partial or complete file left on
// disk by an earlier googet run.
func TestPackageHTTP_ResumeFromDisk(t *testing.T) {
	t.Parallel()
	payload, chksum := testPayload(1000)
	for _, tc := range []struct {
		desc       string
		existing   []byte // Contents written to dst before the download.
		honorRange bool
		wantGETs   []string
	}{
		{
			// An empty destination sends no Range header.
			desc:       "fresh download",
			honorRange: true,
			wantGETs:   []string{"GET "},
		},
		{
			desc:       "resumed download",
			existing:   payload[:400],
			honorRange: true,
			wantGETs:   []string{"GET bytes=400-"},
		},
		{
			// A 200 in reply to a Range request restarts from byte zero
			// within the same attempt.
			desc:     "server ignores range",
			existing: payload[:400],
			wantGETs: []string{"GET bytes=400-"},
		},
		{
			desc:       "already downloaded",
			existing:   payload,
			honorRange: true,
			wantGETs:   nil,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			var (
				mu   sync.Mutex
				gets []string
			)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					writeHead(w, len(payload))
					return
				}
				mu.Lock()
				gets = append(gets, r.Method+" "+r.Header.Get("Range"))
				mu.Unlock()
				start := 0
				if tc.honorRange {
					start = rangeStart(t, r.Header.Get("Range"))
				}
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)-start))
				if start > 0 {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
					w.WriteHeader(http.StatusPartialContent)
				} else {
					w.WriteHeader(http.StatusOK)
				}
				w.Write(payload[start:])
			}))
			defer ts.Close()

			downloader, err := client.NewDownloader("")
			if err != nil {
				t.Fatalf("client.NewDownloader failed: %v", err)
			}
			dst := filepath.Join(t.TempDir(), "pkg.goo")
			if tc.existing != nil {
				if err := os.WriteFile(dst, tc.existing, 0644); err != nil {
					t.Fatalf("os.WriteFile failed: %v", err)
				}
			}
			if err := packageHTTP(context.Background(), ts.URL+"/pkg.goo", dst, chksum, downloader); err != nil {
				t.Fatalf("packageHTTP() = %v, want nil", err)
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("os.ReadFile failed: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("downloaded %d bytes, want %d bytes matching the payload", len(got), len(payload))
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(gets, tc.wantGETs) {
				t.Errorf("GET requests = %q, want %q", gets, tc.wantGETs)
			}
		})
	}
}

func TestPackageHTTP_SlowTrickle(t *testing.T) {
	// Verify that slow trickle downloads complete without being aborted by stall timer.

	payload := []byte("01234567890123456789012345678901234567890123456789")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		chunkSize := 10
		for i := 0; i < len(payload); i += chunkSize {
			end := i + chunkSize
			if end > len(payload) {
				end = len(payload)
			}
			w.Write(payload[i:end])
			if ok {
				flusher.Flush()
			}
			time.Sleep(25 * time.Millisecond)
		}
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 80 * time.Millisecond

	dst := filepath.Join(t.TempDir(), "trickle.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed for slow trickle: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(payload))
	}
}

func TestPackageHTTP_StallAndRangeResume(t *testing.T) {
	// Verify that a stalled stream triggers an HTTP Range resume request and completes.

	payload := []byte("0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	var reqCount int
	var rangeHeaders []string
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		rangeHdr := r.Header.Get("Range")
		if rangeHdr != "" {
			rangeHeaders = append(rangeHeaders, rangeHdr)
		}
		mu.Unlock()

		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}

		flusher, ok := w.(http.Flusher)

		// On first GET request: send partial data (40 bytes) and stall.
		if rangeHdr == "" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:40])
			if ok {
				flusher.Flush()
			}
			// Stall by waiting until client disconnects.
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
			return
		}

		// On Range request: verify range header and serve remainder.
		var start int
		if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-", &start); err != nil {
			http.Error(w, "invalid range", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)-start))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[start:])
		if ok {
			flusher.Flush()
		}
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 50 * time.Millisecond

	dst := filepath.Join(t.TempDir(), "resume.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed on resume: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(rangeHeaders) != 1 {
		t.Errorf("expected 1 range request, got %d: %v", len(rangeHeaders), rangeHeaders)
	} else if rangeHeaders[0] != "bytes=40-" {
		t.Errorf("expected Range: bytes=40-, got %s", rangeHeaders[0])
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(payload))
	}
}

func TestPackageHTTP_MaxRetriesExhausted(t *testing.T) {
	// Verify that download aborts with client.ErrDownloadStalled once the retry budget
	// of consecutive attempts without forward progress is exhausted.

	payload := []byte("0123456789012345678901234567890123456789")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	var attempts int
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}

		mu.Lock()
		attempts++
		mu.Unlock()

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		w.Write([]byte("01234"))
		if ok {
			flusher.Flush()
		}
		// Stall indefinitely until context is cancelled.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 30 * time.Millisecond

	dst := filepath.Join(t.TempDir(), "max_retries.pkg")
	err = packageHTTP(context.Background(), ts.URL, dst, chksum, downloader)
	if err == nil {
		t.Fatal("expected error after retries exhausted, got nil")
	}
	if !errors.Is(err, client.ErrDownloadStalled) {
		t.Errorf("expected error wrapping client.ErrDownloadStalled, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// The first attempt makes progress (5 bytes). Each later attempt receives a
	// 200 OK restart from byte 0 that never gets past byte 5. The first restart
	// records byte 5 as the peak, and maxNoProgressRetries+1 more restarts that
	// do not pass it exhaust the budget: 6 attempts in total.
	if want := maxNoProgressRetries + 3; attempts != want {
		t.Errorf("got %d attempts, want %d (1 initial, 1 restart that sets the peak, %d stuck restarts)", attempts, want, maxNoProgressRetries+1)
	}
}

func TestPackageHTTP_ServerIgnoresRangeAndReturns200OK(t *testing.T) {
	// Verify that a 200 OK response on a range request resets file offset and hash.

	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	var reqCount int
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}

		mu.Lock()
		reqCount++
		currentReq := reqCount
		mu.Unlock()

		flusher, ok := w.(http.Flusher)

		if currentReq == 1 {
			// First attempt: send partial data (20 bytes) and stall.
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:20])
			if ok {
				flusher.Flush()
			}
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
			return
		}

		// Second attempt: server ignores Range and returns 200 OK with entire body.
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
		if ok {
			flusher.Flush()
		}
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 50 * time.Millisecond

	dst := filepath.Join(t.TempDir(), "ignore_range.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed when server returned 200 OK on range: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("expected file length %d, got %d (file was not truncated on 200 OK)", len(payload), len(got))
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(payload))
	}
}

func TestPackageHTTP_NonStallErrorFailsImmediately(t *testing.T) {
	t.Parallel()
	// Verify that non-stall errors fail immediately on first attempt without retrying.
	var attempts int
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "notfound.pkg")
	err = packageHTTP(context.Background(), ts.URL, dst, "dummychksum", downloader)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt on non-stall error, got %d", attempts)
	}
}

func TestPackageHTTP_MultiStallProgressiveResume(t *testing.T) {
	// Verify that multiple successive stalls across retry attempts progressively resume.

	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnopqrstuvwxyz")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	var reqCount int
	var rangeHeaders []string
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}

		mu.Lock()
		reqCount++
		currentAttempt := reqCount
		rangeHdr := r.Header.Get("Range")
		rangeHeaders = append(rangeHeaders, rangeHdr)
		mu.Unlock()

		flusher, ok := w.(http.Flusher)

		switch currentAttempt {
		case 1:
			// Attempt 0: send 25 bytes and stall.
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:25])
			if ok {
				flusher.Flush()
			}
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
		case 2:
			// Attempt 1: expect Range: bytes=25-, send next 25 bytes (25..50) and stall.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 25-%d/%d", len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)-25))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[25:50])
			if ok {
				flusher.Flush()
			}
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
		case 3:
			// Attempt 2: expect Range: bytes=50-, send next 25 bytes (50..75) and stall.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 50-%d/%d", len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)-50))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[50:75])
			if ok {
				flusher.Flush()
			}
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
		case 4:
			// Attempt 3: expect Range: bytes=75-, send final 25 bytes (75..100) and complete cleanly.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 75-%d/%d", len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)-75))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[75:])
			if ok {
				flusher.Flush()
			}
		default:
			t.Errorf("unexpected attempt %d", currentAttempt)
			http.Error(w, "too many attempts", http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 50 * time.Millisecond

	dst := filepath.Join(t.TempDir(), "multi_stall.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed on progressive multi-stall resume: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	expectedRanges := []string{"", "bytes=25-", "bytes=50-", "bytes=75-"}
	if len(rangeHeaders) != len(expectedRanges) {
		t.Fatalf("expected %d requests, got %d: %v", len(expectedRanges), len(rangeHeaders), rangeHeaders)
	}
	for i, want := range expectedRanges {
		if rangeHeaders[i] != want {
			t.Errorf("request %d: expected Range %q, got %q", i, want, rangeHeaders[i])
		}
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("final content mismatch: got %q, want %q", string(got), string(payload))
	}
}

func TestPackageHTTP_200OKFallback_WithSubsequentStall(t *testing.T) {
	// Verify that 200 OK fallback truncates and recovers even if it stalls later.

	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	var reqCount int
	var rangeHeaders []string
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}

		mu.Lock()
		reqCount++
		currentReq := reqCount
		rangeHeaders = append(rangeHeaders, r.Header.Get("Range"))
		mu.Unlock()

		flusher, ok := w.(http.Flusher)

		switch currentReq {
		case 1:
			// Attempt 0: send 20 bytes and stall.
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:20])
			if ok {
				flusher.Flush()
			}
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
		case 2:
			// Attempt 1: server returns 200 OK (ignores Range: bytes=20-), sends 30 bytes from start, then stalls.
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:30])
			if ok {
				flusher.Flush()
			}
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
		case 3:
			// Attempt 2: client resumes Range: bytes=30-, server returns 206 Partial Content with rest.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 30-%d/%d", len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)-30))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[30:])
			if ok {
				flusher.Flush()
			}
		}
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 50 * time.Millisecond

	dst := filepath.Join(t.TempDir(), "200_stall_resume.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	expectedRanges := []string{"", "bytes=20-", "bytes=30-"}
	if len(rangeHeaders) != len(expectedRanges) {
		t.Fatalf("expected %d requests, got %d: %v", len(expectedRanges), len(rangeHeaders), rangeHeaders)
	}
	for i, want := range expectedRanges {
		if rangeHeaders[i] != want {
			t.Errorf("request %d: expected Range %q, got %q", i, want, rangeHeaders[i])
		}
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("expected file length %d, got %d", len(payload), len(got))
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(payload))
	}
}

func TestPackageHTTP_NonStallErrors_Comprehensive(t *testing.T) {
	t.Parallel()
	// Subtest 1: HTTP 500 errors are retried until the no-progress budget is exhausted.
	t.Run("HTTP500_RetriedUntilBudgetExhausted", func(t *testing.T) {
		var attempts atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			attempts.Add(1)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}))
		defer ts.Close()

		downloader, _ := client.NewDownloader("")
		dst := filepath.Join(t.TempDir(), "500.pkg")
		err := packageHTTP(context.Background(), ts.URL, dst, "dummy", downloader)
		if err == nil {
			t.Fatal("expected error on 500, got nil")
		}
		if got := attempts.Load(); got != maxNoProgressRetries+1 {
			t.Errorf("expected %d attempts, got %d", maxNoProgressRetries+1, got)
		}
	})

	// Subtest 2: Checksum mismatch fails immediately and cleans up file from disk.
	t.Run("ChecksumMismatch_DeletesFile", func(t *testing.T) {
		var attempts int
		payload := []byte("actual payload content from server")
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			attempts++
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload)
		}))
		defer ts.Close()

		downloader, _ := client.NewDownloader("")
		dst := filepath.Join(t.TempDir(), "corrupt.pkg")
		err := packageHTTP(context.Background(), ts.URL, dst, "badchecksum00000000000000000000000000000000000000000000000000000000", downloader)
		if err == nil {
			t.Fatal("expected checksum mismatch error, got nil")
		}
		if attempts != 1 {
			t.Errorf("expected exactly 1 attempt, got %d", attempts)
		}
		if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
			t.Errorf("expected destination file to be deleted on checksum mismatch, stat error: %v", statErr)
		}
	})

	// Subtest 3: Context cancellation aborts immediately with zero retries.
	t.Run("ContextCanceled_ZeroRetries", func(t *testing.T) {
		var attempts int
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		downloader, _ := client.NewDownloader("")
		dst := filepath.Join(t.TempDir(), "canceled.pkg")
		err := packageHTTP(ctx, ts.URL, dst, "dummy", downloader)
		if err == nil {
			t.Fatal("expected error on canceled context, got nil")
		}
		if attempts > 1 {
			t.Errorf("expected at most 1 attempt on canceled context, got %d", attempts)
		}
	})
}

func TestPackageHTTP_ExistingFileSkipped(t *testing.T) {
	t.Parallel()
	// Verify that existing file with matching checksum skips all GET requests.
	payload := []byte("already completely downloaded content")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	dst := filepath.Join(t.TempDir(), "existing.pkg")
	if err := os.WriteFile(dst, payload, 0644); err != nil {
		t.Fatalf("failed to write existing file: %v", err)
	}

	var getRequests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getRequests++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	downloader, _ := client.NewDownloader("")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed on existing file: %v", err)
	}

	if getRequests != 0 {
		t.Errorf("expected 0 GET requests when file already exists with matching checksum, got %d", getRequests)
	}
}

func TestPackageHTTP_NoResumeSupportFallback(t *testing.T) {
	// Verify fallback when server does not support range requests.

	payload := []byte("server that does not support range requests at all")
	chksum := fmt.Sprintf("%x", sha256.Sum256(payload))

	var reqCount int
	var rangeHeaders []string
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			// No Accept-Ranges header.
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}

		mu.Lock()
		reqCount++
		currentReq := reqCount
		rangeHeaders = append(rangeHeaders, r.Header.Get("Range"))
		mu.Unlock()

		flusher, ok := w.(http.Flusher)

		if currentReq == 1 {
			// First attempt: send 15 bytes and stall.
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:15])
			if ok {
				flusher.Flush()
			}
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
			return
		}

		// Second attempt: send full file from start.
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
		if ok {
			flusher.Flush()
		}
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 50 * time.Millisecond

	dst := filepath.Join(t.TempDir(), "no_resume.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed when server does not support resume: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	for i, hdr := range rangeHeaders {
		if hdr != "" {
			t.Errorf("request %d should have empty Range header, got %q", i, hdr)
		}
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(payload))
	}
}

// testPayload returns a deterministic payload of n bytes and its SHA256 checksum.
func testPayload(n int) ([]byte, string) {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte('a' + i%26)
	}
	return p, fmt.Sprintf("%x", sha256.Sum256(p))
}

// rangeStart parses the start offset from a "bytes=N-" Range header, returning
// 0 when the header is absent.
func rangeStart(t *testing.T, hdr string) int {
	t.Helper()
	if hdr == "" {
		return 0
	}
	var start int
	if _, err := fmt.Sscanf(hdr, "bytes=%d-", &start); err != nil {
		t.Errorf("invalid Range header %q: %v", hdr, err)
	}
	return start
}

// writeHead answers a HEAD request advertising Range support for size bytes.
func writeHead(w http.ResponseWriter, size int) {
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
}

// writePartialThenStall sends the payload from start as a 200 or 206 response,
// flushes chunk bytes, and then stalls until the client disconnects.
func writePartialThenStall(w http.ResponseWriter, r *http.Request, payload []byte, start, chunk int) {
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)-start))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
	}
	end := start + chunk
	if end > len(payload) {
		end = len(payload)
	}
	w.Write(payload[start:end])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if end == len(payload) {
		return
	}
	select {
	case <-time.After(5 * time.Second):
	case <-r.Context().Done():
	}
}

// hijackAndReset writes a response header plus payload[start:start+chunk] on
// the raw connection and then closes it, simulating a mid-stream disconnect.
func hijackAndReset(t *testing.T, w http.ResponseWriter, payload []byte, start, chunk int) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Errorf("response writer does not support hijacking")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		t.Errorf("Hijack: %v", err)
		return
	}
	defer conn.Close()
	if start > 0 {
		fmt.Fprintf(buf, "HTTP/1.1 206 Partial Content\r\nContent-Range: bytes %d-%d/%d\r\nContent-Length: %d\r\n\r\n", start, len(payload)-1, len(payload), len(payload)-start)
	} else {
		fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(payload))
	}
	end := start + chunk
	if end > len(payload) {
		end = len(payload)
	}
	buf.Write(payload[start:end])
	buf.Flush()
}

func TestPackageHTTP_ConnectionResetResumesWithRange(t *testing.T) {
	t.Parallel()
	// Verify that a mid-stream disconnect (unexpected EOF) is retried with a Range request.
	payload, chksum := testPayload(100)
	var mu sync.Mutex
	var ranges []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			writeHead(w, len(payload))
			return
		}
		hdr := r.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, hdr)
		n := len(ranges)
		mu.Unlock()
		if n == 1 {
			hijackAndReset(t, w, payload, 0, 40)
			return
		}
		writePartialThenStall(w, r, payload, rangeStart(t, hdr), len(payload))
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "reset.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed after connection reset: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"", "bytes=40-"}
	if len(ranges) != len(want) || ranges[0] != want[0] || ranges[1] != want[1] {
		t.Errorf("Range headers = %q, want %q", ranges, want)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch after reset resume")
	}
}

func TestPackageHTTP_FourStallsWithProgressSucceed(t *testing.T) {
	// Verify that the retry budget resets after progress: 4 stalls separated by
	// progress (more than the 3 consecutive no-progress retries) still succeed.
	payload, chksum := testPayload(100)
	var mu sync.Mutex
	var ranges []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			writeHead(w, len(payload))
			return
		}
		hdr := r.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, hdr)
		mu.Unlock()
		writePartialThenStall(w, r, payload, rangeStart(t, hdr), 20)
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 30 * time.Millisecond
	dst := filepath.Join(t.TempDir(), "four_stalls.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed despite progress between stalls: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"", "bytes=20-", "bytes=40-", "bytes=60-", "bytes=80-"}
	if fmt.Sprint(ranges) != fmt.Sprint(want) {
		t.Errorf("Range headers = %q, want %q", ranges, want)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch after multi-stall resume")
	}
}

func TestPackageHTTP_ConsecutiveZeroByteStallsFail(t *testing.T) {
	// Verify that 4 consecutive attempts that deliver zero bytes fail with
	// client.ErrDownloadStalled, using exponential backoff between attempts.
	rec := recordSleepsForTest(t)
	payload, chksum := testPayload(50)
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			writeHead(w, len(payload))
			return
		}
		attempts.Add(1)
		writePartialThenStall(w, r, payload, 0, 0)
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	downloader.StallTimeout = 30 * time.Millisecond
	dst := filepath.Join(t.TempDir(), "zero_stalls.pkg")
	err = packageHTTP(context.Background(), ts.URL, dst, chksum, downloader)
	if !errors.Is(err, client.ErrDownloadStalled) {
		t.Fatalf("packageHTTP() = %v, want error wrapping client.ErrDownloadStalled", err)
	}
	if got := attempts.Load(); got != 4 {
		t.Errorf("got %d GET attempts, want 4", got)
	}
	delays := rec.get()
	wantBase := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(delays) != len(wantBase) {
		t.Fatalf("backoff delays = %v, want %d delays", delays, len(wantBase))
	}
	for i, d := range delays {
		lo := time.Duration(float64(wantBase[i]) * (1 - backoffJitter))
		hi := time.Duration(float64(wantBase[i]) * (1 + backoffJitter))
		if d < lo || d > hi {
			t.Errorf("delay %d = %v, want within [%v, %v]", i, d, lo, hi)
		}
	}
}

func TestPackageHTTP_503ThenSuccess(t *testing.T) {
	t.Parallel()
	// Verify that a 503 response is retried and a later 200 succeeds.
	payload, chksum := testPayload(64)
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			writeHead(w, len(payload))
			return
		}
		if attempts.Add(1) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer ts.Close()

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader failed: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "503.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed after 503: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("got %d GET attempts, want 2", got)
	}
}

func TestPackageHTTP_429IsRetried(t *testing.T) {
	t.Parallel()
	// Verify that a 429 response is retried.
	payload, chksum := testPayload(32)
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer ts.Close()

	downloader, _ := client.NewDownloader("")
	dst := filepath.Join(t.TempDir(), "429.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP failed after 429: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("got %d attempts, want 2", got)
	}
}

func TestPackageHTTP_ParentCancelMidStreamNotRetried(t *testing.T) {
	t.Parallel()
	// Verify that canceling the parent context mid-stream aborts without retrying.
	payload, chksum := testPayload(100)
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			writeHead(w, len(payload))
			return
		}
		attempts.Add(1)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload[:10])
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer ts.Close()

	downloader, _ := client.NewDownloader("")
	dst := filepath.Join(t.TempDir(), "cancel.pkg")
	err := packageHTTP(ctx, ts.URL, dst, chksum, downloader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("packageHTTP() = %v, want error wrapping context.Canceled", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("got %d attempts, want 1", got)
	}
}

func TestPackageHTTP_LowProgressAttemptCap(t *testing.T) {
	// Verify that a source making 1 byte of progress per attempt stops after
	// maxLowProgressAttempts attempts.
	t.Parallel()
	payload, chksum := testPayload(maxLowProgressAttempts + 10)
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			writeHead(w, len(payload))
			return
		}
		attempts.Add(1)
		hijackAndReset(t, w, payload, rangeStart(t, r.Header.Get("Range")), 1)
	}))
	defer ts.Close()

	downloader, _ := client.NewDownloader("")
	dst := filepath.Join(t.TempDir(), "cap.pkg")
	err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("packageHTTP() = %v, want error wrapping io.ErrUnexpectedEOF", err)
	}
	if got := attempts.Load(); got != maxLowProgressAttempts {
		t.Errorf("got %d attempts, want %d", got, maxLowProgressAttempts)
	}
}

func TestPackageHTTP_ProgressingResetsNeverHitCap(t *testing.T) {
	// Verify that 25 connection resets, each after at least minAttemptProgress
	// bytes, do not count toward maxLowProgressAttempts and the download
	// completes.
	t.Parallel()
	const resets = 25
	payload, chksum := testPayload(resets*minAttemptProgress + 100)
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			writeHead(w, len(payload))
			return
		}
		attempts.Add(1)
		hijackAndReset(t, w, payload, rangeStart(t, r.Header.Get("Range")), minAttemptProgress)
	}))
	defer ts.Close()

	downloader, _ := client.NewDownloader("")
	dst := filepath.Join(t.TempDir(), "progressing.pkg")
	if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
		t.Fatalf("packageHTTP() = %v, want nil", err)
	}
	if got := attempts.Load(); got != resets+1 {
		t.Errorf("got %d attempts, want %d", got, resets+1)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch after %d resets", resets)
	}
}

func TestRetryBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// ends lists the offset reached by each failed attempt.
		ends []int64
		// wantExhaustedAt is the 1-based attempt at which the budget is
		// exhausted, or 0 if it never is.
		wantExhaustedAt int
	}{
		{
			name:            "four consecutive zero-progress attempts",
			ends:            []int64{0, 0, 0, 0},
			wantExhaustedAt: 4,
		},
		{
			name:            "progress resets the consecutive counter",
			ends:            []int64{0, 0, 0, 10, 10, 10, 20},
			wantExhaustedAt: 0,
		},
		{
			name:            "twenty low-progress attempts",
			ends:            []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
			wantExhaustedAt: 20,
		},
		{
			name: "high-progress attempts never count",
			ends: func() []int64 {
				var ends []int64
				for i := int64(1); i <= 100; i++ {
					ends = append(ends, i*minAttemptProgress)
				}
				return ends
			}(),
			wantExhaustedAt: 0,
		},
		{
			name: "low-progress attempts accumulate across high-progress ones",
			ends: func() []int64 {
				var ends []int64
				var off int64
				for i := 0; i < maxLowProgressAttempts; i++ {
					off += minAttemptProgress
					ends = append(ends, off)
					off++
					ends = append(ends, off)
				}
				return ends
			}(),
			wantExhaustedAt: 2 * maxLowProgressAttempts,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &retryBudget{}
			got := 0
			for i, end := range tc.ends {
				if err := b.record(end); err != nil {
					got = i + 1
					break
				}
			}
			if got != tc.wantExhaustedAt {
				t.Errorf("budget exhausted at attempt %d, want %d", got, tc.wantExhaustedAt)
			}
		})
	}
}

func TestCheckContentRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value   string
		offset  int64
		wantErr bool
	}{
		{"bytes 40-99/100", 40, false},
		{"bytes 40-99/*", 40, false},
		{"bytes 0-99/100", 40, true},
		{"bytes 41-99/100", 40, true},
		{"", 40, true},
		{"bytes */100", 40, true},
		{"items 40-99/100", 40, true},
	}
	for _, tc := range tests {
		err := checkContentRange(tc.value, tc.offset)
		if (err != nil) != tc.wantErr {
			t.Errorf("checkContentRange(%q, %d) = %v, want error: %v", tc.value, tc.offset, err, tc.wantErr)
		}
		if err != nil && !errors.Is(err, errResumeRejected) {
			t.Errorf("checkContentRange(%q, %d) = %v, want error wrapping errResumeRejected", tc.value, tc.offset, err)
		}
	}
}

func TestPackageHTTP_RejectedResumeRestartsFromScratch(t *testing.T) {
	// Verify that a resume response that does not start at the requested
	// offset, or a 416, discards the partial file and restarts from byte 0.
	t.Parallel()
	tests := []struct {
		name string
		// resume answers the Range request that follows the first reset.
		resume func(w http.ResponseWriter, payload []byte)
	}{
		{
			name: "Content-Range starts at wrong offset",
			resume: func(w http.ResponseWriter, payload []byte) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)))
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(payload)
			},
		},
		{
			name: "Content-Range missing",
			resume: func(w http.ResponseWriter, payload []byte) {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)-40))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(payload[40:])
			},
		},
		{
			name: "416 Range Not Satisfiable",
			resume: func(w http.ResponseWriter, payload []byte) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload, chksum := testPayload(100)
			var mu sync.Mutex
			var ranges []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					writeHead(w, len(payload))
					return
				}
				hdr := r.Header.Get("Range")
				mu.Lock()
				ranges = append(ranges, hdr)
				n := len(ranges)
				mu.Unlock()
				switch n {
				case 1:
					hijackAndReset(t, w, payload, 0, 40)
				case 2:
					tc.resume(w, payload)
				default:
					writePartialThenStall(w, r, payload, rangeStart(t, hdr), len(payload))
				}
			}))
			defer ts.Close()

			downloader, _ := client.NewDownloader("")
			dst := filepath.Join(t.TempDir(), "rejected.pkg")
			if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
				t.Fatalf("packageHTTP() = %v, want nil", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"", "bytes=40-", ""}; fmt.Sprint(ranges) != fmt.Sprint(want) {
				t.Errorf("Range headers = %q, want %q", ranges, want)
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("os.ReadFile failed: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("content mismatch after restart")
			}
		})
	}
}

func TestPackageHTTP_ChecksumMismatchAfterResumeRestartsOnce(t *testing.T) {
	// Verify that a checksum mismatch after a resumed attempt restarts the
	// download from byte 0 once, and fails if it happens again.
	t.Parallel()
	tests := []struct {
		name string
		// corrupt lists the requests whose first 40 bytes are corrupted.
		corrupt    map[int]bool
		wantRanges []string
		wantErr    bool
	}{
		{
			name:       "restart succeeds",
			corrupt:    map[int]bool{1: true},
			wantRanges: []string{"", "bytes=40-", "", "bytes=40-"},
		},
		{
			name:       "second mismatch fails",
			corrupt:    map[int]bool{1: true, 3: true},
			wantRanges: []string{"", "bytes=40-", "", "bytes=40-"},
			wantErr:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload, chksum := testPayload(100)
			bad := append([]byte(nil), payload...)
			for i := 0; i < 40; i++ {
				bad[i] = '!'
			}
			var mu sync.Mutex
			var ranges []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					writeHead(w, len(payload))
					return
				}
				hdr := r.Header.Get("Range")
				mu.Lock()
				ranges = append(ranges, hdr)
				n := len(ranges)
				mu.Unlock()
				if tc.corrupt[n] {
					hijackAndReset(t, w, bad, 0, 40)
					return
				}
				start := rangeStart(t, hdr)
				chunk := len(payload)
				if start == 0 {
					// Force a resume on the next request.
					chunk = 40
				}
				hijackAndReset(t, w, payload, start, chunk)
			}))
			defer ts.Close()

			downloader, _ := client.NewDownloader("")
			dst := filepath.Join(t.TempDir(), "mismatch.pkg")
			err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader)
			var ce *checksumError
			if tc.wantErr != errors.As(err, &ce) {
				t.Fatalf("packageHTTP() = %v, want checksum error: %v", err, tc.wantErr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("packageHTTP() = %v, want nil", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(ranges) != fmt.Sprint(tc.wantRanges) {
				t.Errorf("Range headers = %q, want %q", ranges, tc.wantRanges)
			}
		})
	}
}

func TestRetryBudgetRebase(t *testing.T) {
	t.Parallel()
	t.Run("progress after a restart is not measured against the old offset", func(t *testing.T) {
		b := &retryBudget{}
		if err := b.record(600); err != nil {
			t.Fatalf("record(600) = %v, want nil", err)
		}
		if err := b.rebase(); err != nil {
			t.Fatalf("rebase() = %v, want nil", err)
		}
		for _, end := range []int64{0, 100, 200, 300, 400, 550} {
			if err := b.record(end); err != nil {
				t.Fatalf("record(%d) after rebase = %v, want nil", end, err)
			}
		}
	})
	t.Run("rebase resets the low-progress count", func(t *testing.T) {
		b := &retryBudget{}
		for i := int64(1); i < maxLowProgressAttempts; i++ {
			if err := b.record(i); err != nil {
				t.Fatalf("record(%d) = %v, want nil", i, err)
			}
		}
		if err := b.rebase(); err != nil {
			t.Fatalf("rebase() = %v, want nil", err)
		}
		for i := int64(1); i < maxLowProgressAttempts; i++ {
			if err := b.record(i); err != nil {
				t.Fatalf("record(%d) after rebase = %v, want nil", i, err)
			}
		}
	})
	t.Run("restarts that never pass the previous peak are bounded", func(t *testing.T) {
		b := &retryBudget{}
		restarts := 0
		for ; restarts < 10; restarts++ {
			if err := b.record(50); err != nil {
				t.Fatalf("record(50) = %v, want nil", err)
			}
			if err := b.rebase(); err != nil {
				break
			}
		}
		if want := maxNoProgressRetries + 1; restarts != want {
			t.Errorf("rebase() failed after %d restarts, want %d", restarts, want)
		}
	})
	t.Run("restarts that pass the previous peak are never bounded", func(t *testing.T) {
		b := &retryBudget{}
		for i := int64(1); i <= 50; i++ {
			if err := b.record(i * 10); err != nil {
				t.Fatalf("record(%d) = %v, want nil", i*10, err)
			}
			if err := b.rebase(); err != nil {
				t.Fatalf("rebase() after reaching %d = %v, want nil", i*10, err)
			}
		}
	})
}

func TestPackageHTTP_ProgressAfterRestartIsNotAbandoned(t *testing.T) {
	// Verify that after the partial file is discarded and the download
	// restarts from byte 0, failed attempts that each make progress are
	// retried even though none reaches the offset reached before the restart.
	t.Parallel()
	payload, chksum := testPayload(1000)
	bad := append([]byte(nil), payload...)
	for i := 0; i < 600; i++ {
		bad[i] = '!'
	}
	tests := []struct {
		name string
		// first answers the first two requests, which reach byte 600 and then
		// force a restart from byte 0.
		first func(t *testing.T, w http.ResponseWriter, n int)
	}{
		{
			name: "bad Content-Range",
			first: func(t *testing.T, w http.ResponseWriter, n int) {
				if n == 1 {
					hijackAndReset(t, w, payload, 0, 600)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)))
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(payload)
			},
		},
		{
			name: "checksum mismatch after resume",
			first: func(t *testing.T, w http.ResponseWriter, n int) {
				if n == 1 {
					hijackAndReset(t, w, bad, 0, 600)
					return
				}
				hijackAndReset(t, w, payload, 600, len(payload))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var ranges []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					writeHead(w, len(payload))
					return
				}
				hdr := r.Header.Get("Range")
				mu.Lock()
				ranges = append(ranges, hdr)
				n := len(ranges)
				mu.Unlock()
				start := rangeStart(t, hdr)
				switch {
				case n <= 2:
					tc.first(t, w, n)
				case start < 400:
					// Four attempts after the restart each advance by 100 bytes
					// and then fail, all below byte 600.
					hijackAndReset(t, w, payload, start, 100)
				default:
					writePartialThenStall(w, r, payload, start, len(payload))
				}
			}))
			defer ts.Close()

			downloader, err := client.NewDownloader("")
			if err != nil {
				t.Fatalf("client.NewDownloader failed: %v", err)
			}
			dst := filepath.Join(t.TempDir(), "restart_progress.pkg")
			if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err != nil {
				t.Fatalf("packageHTTP() = %v, want nil", err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"", "bytes=600-", "", "bytes=100-", "bytes=200-", "bytes=300-", "bytes=400-"}
			if fmt.Sprint(ranges) != fmt.Sprint(want) {
				t.Errorf("Range headers = %q, want %q", ranges, want)
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("os.ReadFile failed: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("content mismatch after restart")
			}
		})
	}
}

func TestPackageHTTP_RepeatedRestartsWithoutProgressFail(t *testing.T) {
	// Verify that a source that forces a restart from byte 0 on every resume
	// and never gets past the same byte is eventually abandoned.
	t.Parallel()
	tests := []struct {
		name string
		// resume answers a Range request.
		resume       func(t *testing.T, w http.ResponseWriter, payload []byte)
		wantAttempts int32
	}{
		// A server that ignores Range is covered by
		// TestPackageHTTP_MaxRetriesExhausted.
		{
			name: "server answers every resume with a bad Content-Range",
			resume: func(t *testing.T, w http.ResponseWriter, payload []byte) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)))
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(payload)
			},
			// Each restart takes one rejected resume and one attempt from
			// byte 0, and the first restart sets the peak.
			wantAttempts: 2 * (maxNoProgressRetries + 2),
		},
		{
			name: "server rejects every resume",
			resume: func(t *testing.T, w http.ResponseWriter, payload []byte) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			},
			// Each restart takes one rejected resume and one attempt from
			// byte 0, and the first restart sets the peak.
			wantAttempts: 2 * (maxNoProgressRetries + 2),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload, chksum := testPayload(100)
			var attempts atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					writeHead(w, len(payload))
					return
				}
				attempts.Add(1)
				if r.Header.Get("Range") == "" {
					hijackAndReset(t, w, payload, 0, 50)
					return
				}
				tc.resume(t, w, payload)
			}))
			defer ts.Close()

			downloader, err := client.NewDownloader("")
			if err != nil {
				t.Fatalf("client.NewDownloader failed: %v", err)
			}
			dst := filepath.Join(t.TempDir(), "restart_loop.pkg")
			if err := packageHTTP(context.Background(), ts.URL, dst, chksum, downloader); err == nil {
				t.Fatalf("packageHTTP() = nil, want error")
			}
			if got := attempts.Load(); got != tc.wantAttempts {
				t.Errorf("got %d attempts, want %d", got, tc.wantAttempts)
			}
		})
	}
}

// fakeGCSObject serves payload through gcsRangeOpener. Calls listed in
// stallCalls deliver chunk bytes and then block until the context is canceled.
// Calls listed in rangeErrCalls fail with the HTTP 416 error that GCS returns
// for an offset at or beyond the end of the object.
type fakeGCSObject struct {
	payload       []byte
	chunk         int
	stallCalls    map[int]bool
	rangeErrCalls map[int]bool
	openErr       error

	mu      sync.Mutex
	offsets []int64
	closed  bool
}

// stallingReader returns data and then blocks until ctx is done.
type stallingReader struct {
	ctx  context.Context
	data *bytes.Reader
}

func (s *stallingReader) Read(p []byte) (int, error) {
	if s.data.Len() > 0 {
		return s.data.Read(p)
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func (s *stallingReader) Close() error { return nil }

// newRangeReader mimics ObjectHandle.NewRangeReader with length -1.
func (f *fakeGCSObject) newRangeReader(ctx context.Context, offset int64) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	f.offsets = append(f.offsets, offset)
	call := len(f.offsets)
	f.mu.Unlock()
	if f.openErr != nil {
		return nil, 0, f.openErr
	}
	if f.rangeErrCalls[call] {
		return nil, 0, &googleapi.Error{Code: http.StatusRequestedRangeNotSatisfiable, Message: "The requested range cannot be satisfied."}
	}
	if f.stallCalls[call] {
		end := int(offset) + f.chunk
		if end > len(f.payload) {
			end = len(f.payload)
		}
		return &stallingReader{ctx: ctx, data: bytes.NewReader(f.payload[offset:end])}, int64(len(f.payload)), nil
	}
	return io.NopCloser(bytes.NewReader(f.payload[offset:])), int64(len(f.payload)), nil
}

func (f *fakeGCSObject) newOpener(ctx context.Context, bucket, object string) (opener, func() error, error) {
	closeFn := func() error {
		f.mu.Lock()
		f.closed = true
		f.mu.Unlock()
		return nil
	}
	return gcsRangeOpener(f.newRangeReader), closeFn, nil
}

// useFakeGCS replaces newGCSOpener with f for the duration of t.
func useFakeGCS(t *testing.T, f *fakeGCSObject) {
	t.Helper()
	orig := newGCSOpener
	newGCSOpener = f.newOpener
	t.Cleanup(func() { newGCSOpener = orig })
}

func TestPackageGCS_StallResumesWithRangeReader(t *testing.T) {
	// Verify that a stalled GCS read is retried and resumed at the partial offset.
	payload, chksum := testPayload(90)
	f := &fakeGCSObject{payload: payload, chunk: 30, stallCalls: map[int]bool{1: true, 2: true}}
	useFakeGCS(t, f)

	dst := filepath.Join(t.TempDir(), "gcs.pkg")
	downloader := &client.Downloader{StallTimeout: 30 * time.Millisecond}
	if err := Package(context.Background(), "gs://bucket/obj.goo", dst, chksum, downloader); err != nil {
		t.Fatalf("Package(gs://) failed: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []int64{0, 30, 60}; fmt.Sprint(f.offsets) != fmt.Sprint(want) {
		t.Errorf("GCS read offsets = %v, want %v", f.offsets, want)
	}
	if !f.closed {
		t.Error("GCS client was not closed")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch after GCS resume")
	}
}

func TestPackageGCS_RangeErrorRestartsFromScratch(t *testing.T) {
	// Verify that a GCS range error on a resumed read discards the partial
	// file and restarts from offset 0.
	payload, chksum := testPayload(90)
	f := &fakeGCSObject{payload: payload, chunk: 30, stallCalls: map[int]bool{1: true}, rangeErrCalls: map[int]bool{2: true}}
	useFakeGCS(t, f)

	dst := filepath.Join(t.TempDir(), "gcs_range.pkg")
	downloader := &client.Downloader{StallTimeout: 30 * time.Millisecond}
	if err := Package(context.Background(), "gs://bucket/obj.goo", dst, chksum, downloader); err != nil {
		t.Fatalf("Package(gs://) failed: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []int64{0, 30, 0}; fmt.Sprint(f.offsets) != fmt.Sprint(want) {
		t.Errorf("GCS read offsets = %v, want %v", f.offsets, want)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content mismatch after GCS restart")
	}
}

func TestGCSRangeOpener(t *testing.T) {
	// Verify that only a range error on a resumed read is mapped to
	// errResumeRejected.
	t.Parallel()
	rangeErr := &googleapi.Error{Code: http.StatusRequestedRangeNotSatisfiable}
	open := gcsRangeOpener(func(context.Context, int64) (io.ReadCloser, int64, error) { return nil, 0, rangeErr })
	if _, _, _, err := open(context.Background(), 10); !errors.Is(err, errResumeRejected) {
		t.Errorf("open(offset 10) = %v, want error wrapping errResumeRejected", err)
	}
	if _, _, _, err := open(context.Background(), 0); errors.Is(err, errResumeRejected) || !errors.Is(err, rangeErr) {
		t.Errorf("open(offset 0) = %v, want the original range error", err)
	}
}

func TestPackageGCS_ChecksumMismatchNotRetried(t *testing.T) {
	// Verify that a GCS checksum mismatch fails once and deletes the file.
	payload, _ := testPayload(40)
	f := &fakeGCSObject{payload: payload}
	useFakeGCS(t, f)

	dst := filepath.Join(t.TempDir(), "gcs_bad.pkg")
	if err := Package(context.Background(), "gs://bucket/obj.goo", dst, "bad", nil); err == nil {
		t.Fatal("Package(gs://) succeeded with bad checksum, want error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.offsets) != 1 {
		t.Errorf("got %d GCS reads, want 1", len(f.offsets))
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("bad file not deleted: %v", err)
	}
}

func TestPackageGCS_NonRetryableOpenError(t *testing.T) {
	// Verify that a non-transient GCS open error is returned without retrying.
	f := &fakeGCSObject{openErr: errors.New("storage: object doesn't exist")}
	useFakeGCS(t, f)

	dst := filepath.Join(t.TempDir(), "gcs_missing.pkg")
	if err := Package(context.Background(), "gs://bucket/obj.goo", dst, "x", nil); err == nil {
		t.Fatal("Package(gs://) succeeded, want error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.offsets) != 1 {
		t.Errorf("got %d GCS opens, want 1", len(f.offsets))
	}
}

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsRetryable(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nil", context.Background(), nil, false},
		{"stalled", context.Background(), fmt.Errorf("x: %w", client.ErrDownloadStalled), true},
		{"unexpected EOF", context.Background(), io.ErrUnexpectedEOF, true},
		{"ECONNRESET", context.Background(), &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}, true},
		{"ECONNABORTED", context.Background(), syscall.ECONNABORTED, true},
		{"EPIPE", context.Background(), syscall.EPIPE, true},
		{"net timeout", context.Background(), timeoutError{}, true},
		{"http2 stream error", context.Background(), errors.New("stream error: stream ID 3; INTERNAL_ERROR"), true},
		{"http2 GOAWAY", context.Background(), errors.New("http2: server sent GOAWAY and closed the connection"), true},
		{"windows reset", context.Background(), errors.New("wsarecv: An existing connection was forcibly closed by the remote host"), true},
		{"503", context.Background(), &statusError{code: 503, status: "503 Service Unavailable"}, true},
		{"429", context.Background(), &statusError{code: 429, status: "429 Too Many Requests"}, true},
		{"404", context.Background(), &statusError{code: 404, status: "404 Not Found"}, false},
		{"disk write EPIPE", context.Background(), &writeError{err: syscall.EPIPE}, false},
		{"disk full", context.Background(), &writeError{err: errors.New("no space left on device")}, false},
		{"parent canceled", canceled, client.ErrDownloadStalled, false},
		{"other", context.Background(), errors.New("x509: certificate signed by unknown authority"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.ctx, tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		noProgress int
		base       time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{6, 30 * time.Second},
		{100, 30 * time.Second},
	}
	for _, tc := range tests {
		for i := 0; i < 20; i++ {
			d := backoffDelay(tc.noProgress)
			lo := time.Duration(float64(tc.base) * (1 - backoffJitter))
			hi := time.Duration(float64(tc.base) * (1 + backoffJitter))
			if d < lo || d > hi {
				t.Fatalf("backoffDelay(%d) = %v, want within [%v, %v]", tc.noProgress, d, lo, hi)
			}
		}
	}
}

func TestSleepHonorsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := realSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep(canceled ctx) = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("sleep did not return promptly on canceled context")
	}
	if err := realSleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleep(1ms) = %v, want nil", err)
	}
}
