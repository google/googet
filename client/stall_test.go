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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestStallReaderStalls(t *testing.T) {
	pr, pw := io.Pipe()
	stalled := make(chan struct{})
	r := NewStallReader(pr, 20*time.Millisecond, func() {
		close(stalled)
		pw.CloseWithError(context.Canceled)
	})
	defer r.Close()
	_, err := io.Copy(io.Discard, r)
	select {
	case <-stalled:
	default:
		t.Fatal("onStall was not called")
	}
	if !errors.Is(err, ErrStalled) || !errors.Is(err, context.Canceled) {
		t.Errorf("io.Copy() = %v, want it to wrap ErrStalled and context.Canceled", err)
	}
}

func TestStallReaderAllowsSlowProgress(t *testing.T) {
	const timeout = 100 * time.Millisecond
	pr, pw := io.Pipe()
	go func() {
		// Trickle one byte every timeout/5 for 4x the timeout.
		for range 20 {
			time.Sleep(timeout / 5)
			pw.Write([]byte{'x'})
		}
		pw.Close()
	}()
	r := NewStallReader(pr, timeout, func() {
		t.Error("onStall called for a transfer that kept receiving data")
		pw.CloseWithError(context.Canceled)
	})
	defer r.Close()
	if n, err := io.Copy(io.Discard, r); err != nil || n != 20 {
		t.Errorf("io.Copy() = %d, %v, want 20, nil", n, err)
	}
}

func TestStallReaderStopsTimer(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		desc string
		use  func(*testing.T, io.ReadCloser)
	}{
		{"close", func(t *testing.T, r io.ReadCloser) { r.Close() }},
		{"read error", func(t *testing.T, r io.ReadCloser) {
			if _, err := io.Copy(io.Discard, r); err != boom {
				t.Errorf("io.Copy() = %v, want %v", err, boom)
			}
		}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			pr, pw := io.Pipe()
			pw.CloseWithError(boom)
			const timeout = 20 * time.Millisecond
			r := NewStallReader(pr, timeout, func() { t.Error("onStall called after the timer was stopped") })
			tc.use(t, r)
			time.Sleep(5 * timeout)
		})
	}
}

func TestStallReaderEOFAfterTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	r := NewStallReader(pr, 20*time.Millisecond, func() { pw.Close() })
	defer r.Close()
	if n, err := io.Copy(io.Discard, r); err != nil || n != 0 {
		t.Errorf("io.Copy() = %d, %v, want 0, nil for a transfer that ended", n, err)
	}
}

func TestNewDownloaderResponseHeaderTimeout(t *testing.T) {
	d, err := NewDownloader("")
	if err != nil {
		t.Fatalf(`NewDownloader("") failed: %v`, err)
	}
	if got := d.HTTPClient.Transport.(*http.Transport).ResponseHeaderTimeout; got != DefaultStallTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", got, DefaultStallTimeout)
	}
}

func TestUnmarshalRepoPackagesHTTPStall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	d := &Downloader{HTTPClient: &http.Client{}, StallTimeout: 100 * time.Millisecond}
	_, err := d.unmarshalRepoPackagesHTTP(context.Background(), srv.URL, filepath.Join(t.TempDir(), "cache.rs"))
	if !errors.Is(err, ErrStalled) {
		t.Errorf("unmarshalRepoPackagesHTTP() = %v, want ErrStalled", err)
	}
}
