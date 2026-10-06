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

func TestWatchStallCancelsWithoutProgress(t *testing.T) {
	ctx, sw := WatchStall(context.Background(), 20*time.Millisecond)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context not canceled after the stall timeout")
	}
	err := ctx.Err()
	sw.Stop(&err)
	if !errors.Is(err, ErrStalled) || !errors.Is(err, context.Canceled) {
		t.Errorf("Stop() set err = %v, want it to wrap ErrStalled and context.Canceled", err)
	}
}

func TestWatchStallAllowsSlowProgress(t *testing.T) {
	const timeout = 100 * time.Millisecond
	ctx, sw := WatchStall(context.Background(), timeout)
	var err error
	defer sw.Stop(&err)
	pr, pw := io.Pipe()
	go func() {
		// Trickle one byte every timeout/5 for 4x the timeout.
		for range 20 {
			time.Sleep(timeout / 5)
			pw.Write([]byte{'x'})
		}
		pw.Close()
	}()
	n, err := io.Copy(io.Discard, sw.Reader(pr))
	if err != nil || n != 20 {
		t.Fatalf("io.Copy() = %d, %v, want 20, nil", n, err)
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("ctx.Err() = %v (cause %v), want nil for a transfer that kept making progress", err, context.Cause(ctx))
	}
}

func TestWatchStallProgressResetsTimer(t *testing.T) {
	const timeout = 100 * time.Millisecond
	ctx, sw := WatchStall(context.Background(), timeout)
	var err error
	defer sw.Stop(&err)
	// Report progress every timeout/2 for 3x the timeout.
	for range 6 {
		time.Sleep(timeout / 2)
		sw.Progress()
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("ctx.Err() = %v (cause %v), want nil while Progress is called", err, context.Cause(ctx))
	}
}

func TestStopPassesOtherErrorsThrough(t *testing.T) {
	_, sw := WatchStall(context.Background(), time.Hour)
	want := errors.New("boom")
	err := want
	sw.Stop(&err)
	if err != want {
		t.Errorf("Stop() set err = %v, want %v", err, want)
	}

	_, sw = WatchStall(context.Background(), time.Nanosecond)
	time.Sleep(10 * time.Millisecond)
	err = nil
	sw.Stop(&err)
	if err != nil {
		t.Errorf("Stop() set nil err = %v, want nil", err)
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
