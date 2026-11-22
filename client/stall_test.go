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
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
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

// TestStallReaderAllowsSlowProgress runs in a synctest bubble so the gaps
// between bytes are exact in fake time and a slow runner can't stretch them
// past the timeout.
func TestStallReaderAllowsSlowProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			timeout = 250 * time.Millisecond
			gap     = timeout * 9 / 10
			chunks  = 5
		)
		// The transfer must outlast the timeout, so a timer that data doesn't
		// restart fires.
		if chunks*gap <= timeout {
			t.Fatalf("%d chunks %v apart take %v, want more than the %v timeout", chunks, gap, chunks*gap, timeout)
		}
		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer pw.Close()
			// Send one byte per gap; each gap is shorter than the timeout.
			for range chunks {
				time.Sleep(gap)
				if _, err := pw.Write([]byte{'x'}); err != nil {
					return
				}
			}
		}()
		// The bubble must not end while the writer sleeps.
		defer func() { <-done }()
		var stalled atomic.Bool
		r := NewStallReader(pr, timeout, func() {
			stalled.Store(true)
			pw.CloseWithError(context.Canceled)
		})
		defer r.Close()
		start := time.Now()
		if n, err := io.Copy(io.Discard, r); err != nil || n != chunks {
			t.Errorf("io.Copy() = %d, %v, want %d, nil", n, err, chunks)
		}
		if got, want := time.Since(start), chunks*gap; got != want {
			t.Errorf("io.Copy() took %v, want %v", got, want)
		}
		if stalled.Load() {
			t.Error("onStall called for a transfer that kept receiving data")
		}
	})
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
			const timeout = 50 * time.Millisecond
			var called atomic.Bool
			r := NewStallReader(pr, timeout, func() { called.Store(true) })
			tc.use(t, r)
			time.Sleep(3 * timeout)
			if called.Load() {
				t.Error("onStall called after the timer was stopped")
			}
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

func TestStallReaderDefaultTimeout(t *testing.T) {
	setStallTimeout(t, time.Hour)
	for _, timeout := range []time.Duration{0, -time.Second} {
		var stalled atomic.Bool
		pr, _ := io.Pipe()
		r := NewStallReader(pr, timeout, func() { stalled.Store(true) })
		time.Sleep(50 * time.Millisecond)
		r.Close()
		if stalled.Load() {
			t.Errorf("NewStallReader(%v) stalled at once, want StallTimeout", timeout)
		}
	}

	setStallTimeout(t, 20*time.Millisecond)
	stalled := make(chan struct{})
	pr, _ := io.Pipe()
	r := NewStallReader(pr, 0, func() { close(stalled) })
	defer r.Close()
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		t.Error("NewStallReader(0) did not stall after StallTimeout")
	}
}

// setStallTimeout sets StallTimeout for the rest of the test.
func setStallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := StallTimeout
	StallTimeout = d
	t.Cleanup(func() { StallTimeout = old })
}

func TestNewDownloaderResponseHeaderTimeout(t *testing.T) {
	setStallTimeout(t, 5*time.Minute)
	d, err := NewDownloader("")
	if err != nil {
		t.Fatalf(`NewDownloader("") failed: %v`, err)
	}
	if got := d.HTTPClient.Transport.(*http.Transport).ResponseHeaderTimeout; got != 5*time.Minute {
		t.Errorf("ResponseHeaderTimeout = %v, want StallTimeout (5m0s)", got)
	}
	if d.StallTimeout != 5*time.Minute {
		t.Errorf("d.StallTimeout = %v, want StallTimeout (5m0s)", d.StallTimeout)
	}

	setStallTimeout(t, -time.Second)
	d, err = NewDownloader("")
	if err != nil {
		t.Fatalf(`NewDownloader("") with negative StallTimeout failed: %v`, err)
	}
	if got := d.HTTPClient.Transport.(*http.Transport).ResponseHeaderTimeout; got != DefaultStallTimeout {
		t.Errorf("ResponseHeaderTimeout with negative StallTimeout = %v, want %v", got, DefaultStallTimeout)
	}
	if d.StallTimeout != DefaultStallTimeout {
		t.Errorf("d.StallTimeout with negative StallTimeout = %v, want %v", d.StallTimeout, DefaultStallTimeout)
	}
}

func TestOpenGCS(t *testing.T) {
	setStallTimeout(t, 50*time.Millisecond)
	ok := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }
	slowOK := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte("hello"))
	}
	hang := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	for _, tc := range []struct {
		desc           string
		timeout        time.Duration
		defaultTimeout time.Duration
		handler        http.HandlerFunc
		wantErrs       []error
	}{
		{"ok", 5 * time.Second, 0, ok, nil},
		{"zero timeout uses StallTimeout", 0, 0, hang, []error{ErrStalled}},
		{"negative timeout uses StallTimeout", -time.Second, 0, hang, []error{ErrStalled}},
		{"zero timeout does not stall at once", 0, time.Hour, slowOK, nil},
		{"negative timeout does not stall at once", -time.Second, time.Hour, slowOK, nil},
		{"open stalls", 100 * time.Millisecond, 0, hang, []error{ErrStalled}},
		{"body stalls", 100 * time.Millisecond, 0, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "5")
			w.Write([]byte("he"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}, []error{ErrStalled}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.defaultTimeout != 0 {
				setStallTimeout(t, tc.defaultTimeout)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
			gcs, err := storage.NewClient(ctx, option.WithoutAuthentication())
			if err != nil {
				t.Fatalf("storage.NewClient() failed: %v", err)
			}
			defer gcs.Close()
			d := &Downloader{StallTimeout: tc.timeout}
			body, size, err := d.OpenGCS(ctx, gcs.Bucket("bucket").Object("object"))
			var got []byte
			if err == nil {
				got, err = io.ReadAll(body)
				body.Close()
			}
			if len(tc.wantErrs) == 0 && err != nil {
				t.Fatalf("OpenGCS() and reading the body = %v, want nil", err)
			}
			for _, want := range tc.wantErrs {
				if !errors.Is(err, want) {
					t.Errorf("OpenGCS() and reading the body = %v, want it to wrap %v", err, want)
				}
			}
			if len(tc.wantErrs) == 0 && (size != 5 || string(got) != "hello") {
				t.Errorf("OpenGCS() = %q, size %d, want %q, size 5", got, size, "hello")
			}
		})
	}
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestOpenGCSRetryStalls checks that a stall while the GCS client backs off
// between retries of a failing open cancels the retries, and that the error
// keeps the last server error alongside the stall cause. It runs in a synctest
// bubble, where requests take no time and the fake clock advances only once
// every goroutine is blocked, so the stall always fires during a backoff. With
// a real clock and server, the stall can instead cancel a request in flight,
// which fails with net/http's error wrapping only ErrStalled.
func TestOpenGCSRetryStalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		unavailable := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{
				Status:     "503 Service Unavailable",
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{},
				Body:       http.NoBody,
				Request:    r,
			}, nil
		})
		// A stalled retry that is not canceled runs until this deadline and
		// fails without ErrStalled.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t.Setenv("STORAGE_EMULATOR_HOST", "http://gcs.invalid")
		gcs, err := storage.NewClient(ctx, option.WithHTTPClient(&http.Client{Transport: unavailable}))
		if err != nil {
			t.Fatalf("storage.NewClient() failed: %v", err)
		}
		defer gcs.Close()
		d := &Downloader{StallTimeout: 50 * time.Millisecond}
		start := time.Now()
		_, _, err = d.OpenGCS(ctx, gcs.Bucket("bucket").Object("object"))
		if elapsed := time.Since(start); elapsed != d.StallTimeout {
			t.Errorf("OpenGCS() returned after %v, want %v", elapsed, d.StallTimeout)
		}
		if requests.Load() == 0 {
			t.Error("OpenGCS() sent no requests")
		}
		for _, want := range []error{ErrStalled, context.Canceled} {
			if !errors.Is(err, want) {
				t.Errorf("OpenGCS() = %v, want it to wrap %v", err, want)
			}
		}
		var apiErr *googleapi.Error
		if !errors.As(err, &apiErr) || apiErr.Code != http.StatusServiceUnavailable {
			t.Errorf("OpenGCS() = %v, want it to wrap a googleapi.Error with code %d", err, http.StatusServiceUnavailable)
		}
	})
}

func TestUnmarshalRepoPackagesHTTPStall(t *testing.T) {
	for _, tc := range []struct {
		desc    string
		handler http.HandlerFunc
	}{
		{
			desc: "index.gz stalls before gzip header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			},
		},
		{
			desc: "index.gz stalls after gzip header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				gw := gzip.NewWriter(w)
				gw.Flush()
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			},
		},
		{
			desc: "plain index stalls",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/index.gz") {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			cf := filepath.Join(t.TempDir(), "cache.rs")
			d := &Downloader{HTTPClient: &http.Client{}, StallTimeout: 50 * time.Millisecond}
			_, err := d.unmarshalRepoPackagesHTTP(context.Background(), srv.URL, cf)
			if !errors.Is(err, ErrStalled) {
				t.Errorf("unmarshalRepoPackagesHTTP() = %v, want ErrStalled", err)
			}
			if _, err := os.Stat(cf); !os.IsNotExist(err) {
				t.Errorf("os.Stat(%q) after stall = %v, want IsNotExist", cf, err)
			}
		})
	}
}

func TestUnmarshalRepoPackagesHTTPTruncated(t *testing.T) {
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write([]byte(`[{"Source": "a"}]`))
	gw.Close()
	for _, tc := range []struct {
		desc string
		body []byte
		gzip bool
	}{
		{"gzip", gz.Bytes()[:gz.Len()/2], true},
		{"plain", []byte(`[{"Source": "a"`), false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !tc.gzip && strings.HasSuffix(r.URL.Path, "/index.gz") {
					http.NotFound(w, r)
					return
				}
				w.Write(tc.body)
			}))
			defer srv.Close()
			cf := filepath.Join(t.TempDir(), "cache.rs")
			d := &Downloader{HTTPClient: &http.Client{}, StallTimeout: 5 * time.Second}
			if _, err := d.unmarshalRepoPackagesHTTP(context.Background(), srv.URL, cf); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("unmarshalRepoPackagesHTTP() = %v, want io.ErrUnexpectedEOF", err)
			}
			if _, err := os.Stat(cf); !os.IsNotExist(err) {
				t.Errorf("os.Stat(%q) after a truncated index = %v, want IsNotExist", cf, err)
			}
		})
	}
}

func TestUnmarshalRepoPackagesGCS(t *testing.T) {
	t.Run("index.gz stall does not fall back to index", func(t *testing.T) {
		var reqs atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			<-r.Context().Done()
		}))
		defer srv.Close()
		t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
		d := &Downloader{StallTimeout: 50 * time.Millisecond}
		_, err := d.unmarshalRepoPackagesGCS(context.Background(), "bkt", "", "gs://bkt", filepath.Join(t.TempDir(), "cache.rs"))
		if !errors.Is(err, ErrStalled) {
			t.Errorf("unmarshalRepoPackagesGCS() = %v, want ErrStalled", err)
		}
		if got := reqs.Load(); got != 1 {
			t.Errorf("got %d GCS requests, want 1", got)
		}
	})

	t.Run("non-404 index.gz error does not fall back to index", func(t *testing.T) {
		var reqs atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
		d := &Downloader{StallTimeout: 5 * time.Second}
		_, err := d.unmarshalRepoPackagesGCS(context.Background(), "bkt", "", "gs://bkt", filepath.Join(t.TempDir(), "cache.rs"))
		if err == nil {
			t.Error("unmarshalRepoPackagesGCS() = nil, want 403 error")
		}
		if got := reqs.Load(); got != 1 {
			t.Errorf("got %d GCS requests, want 1", got)
		}
	})

	t.Run("plain index stall does not write cache", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "index.gz") {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer srv.Close()
		t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
		cf := filepath.Join(t.TempDir(), "cache.rs")
		d := &Downloader{StallTimeout: 50 * time.Millisecond}
		_, err := d.unmarshalRepoPackagesGCS(context.Background(), "bkt", "", "gs://bkt", cf)
		if !errors.Is(err, ErrStalled) {
			t.Errorf("unmarshalRepoPackagesGCS() = %v, want ErrStalled", err)
		}
		if _, err := os.Stat(cf); !os.IsNotExist(err) {
			t.Errorf("os.Stat(%q) after stall = %v, want IsNotExist", cf, err)
		}
	})

	t.Run("missing index.gz falls back to index", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "index.gz") {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte("[]"))
		}))
		defer srv.Close()
		t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
		d := &Downloader{StallTimeout: 5 * time.Second}
		got, err := d.unmarshalRepoPackagesGCS(context.Background(), "bkt", "", "gs://bkt", filepath.Join(t.TempDir(), "cache.rs"))
		if err != nil || len(got) != 0 {
			t.Errorf("unmarshalRepoPackagesGCS() = %v, %v, want [], nil", got, err)
		}
	})
}
