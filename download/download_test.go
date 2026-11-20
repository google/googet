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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

func init() {
	logger.Init("test", true, false, io.Discard)
}

func TestDownload(t *testing.T) {
	r := bytes.NewReader([]byte("some content"))
	tempDir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	chksum := goolib.Checksum(r)
	if _, err := r.Seek(0, 0); err != nil {
		t.Errorf("error seeking to front of reader: %v", err)
	}
	tempFile := path.Join(tempDir, "test")
	if err := download(r, int64(r.Len()), tempFile, chksum); err != nil {
		t.Errorf("error downloading and checking checksum: %v", err)
	}
	if err := download(r, int64(r.Len()), tempFile, "notachecksum"); err == nil {
		t.Error("wanted but did not recieve checksum error")
	}
}

func TestDownloadUnknownSize(t *testing.T) {
	content := []byte("some content")
	chksum := goolib.Checksum(bytes.NewReader(content))
	tempFile := path.Join(t.TempDir(), "test")
	// A size of 0 means the total is unknown; the checksum must still verify.
	if err := download(bytes.NewReader(content), 0, tempFile, chksum); err != nil {
		t.Errorf("error downloading with unknown size: %v", err)
	}
	got, err := os.ReadFile(tempFile)
	if err != nil {
		t.Fatalf("error reading downloaded file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded contents = %q, want %q", got, content)
	}
}

// failingReader returns data and then err.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestDownloadFailureRemovesFile checks that download, which the GCS path
// uses, removes dst on any error, so no empty, partial or corrupt file is left.
func TestDownloadFailureRemovesFile(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	readErr := errors.New("connection reset")
	for _, tc := range []struct {
		desc    string
		r       io.Reader
		chksum  string
		wantErr error
	}{
		{desc: "checksum mismatch", r: bytes.NewReader(payload), chksum: "badsum"},
		{desc: "read fails partway", r: &failingReader{data: payload[:400], err: readErr}, chksum: chksum, wantErr: readErr},
		{desc: "read fails before any data", r: &failingReader{err: readErr}, chksum: chksum, wantErr: readErr},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "pkg.goo")
			err := download(tc.r, int64(len(payload)), dst, tc.chksum)
			if err == nil {
				t.Fatal("download() = nil, want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("download() = %v, want %v", err, tc.wantErr)
			}
			assertNoFile(t, dst)
		})
	}
}

// rangeServer serves payload, advertising range support on HEAD. When
// honorRange is false it answers ranged GETs with the whole file and 200 OK,
// as some proxies do. When rejectHEAD is true it answers HEAD with 405.
func rangeServer(t *testing.T, payload []byte, honorRange, rejectHEAD bool, requests *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.Method+" "+r.Header.Get("Range"))
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead && rejectHEAD {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			return
		}
		if rng := r.Header.Get("Range"); rng != "" && honorRange {
			start, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
			if err != nil {
				t.Errorf("bad Range header %q: %v", rng, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if start >= len(payload) {
				w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(len(payload)))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[start:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Write(payload)
	}))
}

// methodServer answers HEAD requests with head and GET requests with get,
// and counts the requests it receives per method.
type methodServer struct {
	*httptest.Server
	mu     sync.Mutex
	counts map[string]int
}

func newMethodServer(t *testing.T, head, get http.HandlerFunc) *methodServer {
	t.Helper()
	s := &methodServer{counts: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.counts[r.Method]++
		s.mu.Unlock()
		switch r.Method {
		case http.MethodHead:
			head(w, r)
		case http.MethodGet:
			get(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *methodServer) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[method]
}

// statusHandler answers every request with code and no body.
func statusHandler(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// headOK answers HEAD with the payload size, advertising range support only
// when ranges is true.
func headOK(size int, ranges bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if ranges {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Content-Length", strconv.Itoa(size))
	}
}

// resetConn closes the connection before sending any response headers.
func resetConn(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(err)
	}
	if tc, ok := conn.(interface{ SetLinger(int) error }); ok {
		tc.SetLinger(0) // Send RST rather than FIN.
	}
	conn.Close()
}

// assertNoFile fails the test if path exists.
func assertNoFile(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err == nil {
		t.Errorf("%s exists with %d bytes after a failed download, want no file", filepath.Base(path), fi.Size())
	} else if !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want IsNotExist", path, err)
	}
}

func TestPackageHTTP(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	for _, tc := range []struct {
		desc       string
		existing   []byte // Contents written to dst before the download.
		honorRange bool
		rejectHEAD bool
		wantGETs   []string
	}{
		{
			// An empty destination still asks for bytes=0-; the server may
			// answer 200 or 206 and either way nothing is on disk to keep.
			desc:     "fresh download",
			wantGETs: []string{"GET bytes=0-"},
		},
		{
			desc:       "resumed download",
			existing:   payload[:400],
			honorRange: true,
			wantGETs:   []string{"GET bytes=400-"},
		},
		{
			desc:     "server ignores range",
			existing: payload[:400],
			wantGETs: []string{"GET bytes=400-"},
		},
		{
			// A failed HEAD may only mean the server rejects HEAD, so the
			// partial file is still resumed.
			desc:       "HEAD rejected",
			existing:   payload[:400],
			honorRange: true,
			rejectHEAD: true,
			wantGETs:   []string{"GET bytes=400-"},
		},
		{
			desc:       "HEAD rejected and server ignores range",
			existing:   payload[:400],
			rejectHEAD: true,
			wantGETs:   []string{"GET bytes=400-"},
		},
		{
			desc:       "HEAD rejected and existing file not smaller",
			existing:   bytes.Repeat([]byte("x"), len(payload)),
			honorRange: true,
			rejectHEAD: true,
			wantGETs:   []string{"GET bytes=1000-", "GET "},
		},
		{
			desc:     "already downloaded",
			existing: payload,
			wantGETs: nil,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var requests []string
			srv := rangeServer(t, payload, tc.honorRange, tc.rejectHEAD, &requests)
			defer srv.Close()

			dst := filepath.Join(t.TempDir(), "pkg.goo")
			if tc.existing != nil {
				if err := os.WriteFile(dst, tc.existing, 0644); err != nil {
					t.Fatalf("writing existing file: %v", err)
				}
			}
			if err := packageHTTP(context.Background(), srv.URL+"/pkg.goo", dst, chksum, downloader); err != nil {
				t.Fatalf("packageHTTP() = %v, want nil", err)
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("reading downloaded file: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("downloaded %d bytes, want %d bytes matching the payload", len(got), len(payload))
			}
			var gets []string
			for _, r := range requests {
				if strings.HasPrefix(r, "GET") {
					gets = append(gets, r)
				}
			}
			if !slices.Equal(gets, tc.wantGETs) {
				t.Errorf("GET requests = %q, want %q", gets, tc.wantGETs)
			}
		})
	}

	t.Run("checksum mismatch removes file", func(t *testing.T) {
		var requests []string
		srv := rangeServer(t, payload, true, false, &requests)
		defer srv.Close()
		dst := filepath.Join(t.TempDir(), "pkg.goo")
		if err := packageHTTP(context.Background(), srv.URL+"/pkg.goo", dst, "badsum", downloader); err == nil {
			t.Fatal("packageHTTP() with bad checksum = nil, want error")
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("os.Stat(dst) after checksum mismatch = %v, want IsNotExist", err)
		}
	})
}

// TestPackageHTTPFailureLeavesNoEmptyFile checks that a failed HTTP download
// never leaves a 0-byte file at dst, however dst came to be empty.
func TestPackageHTTPFailureLeavesNoEmptyFile(t *testing.T) {
	setRetryDelay(t, func(int) time.Duration { return 0 })
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	for _, tc := range []struct {
		desc     string
		existing []byte // Written to dst before the download; nil means no file.
		head     http.HandlerFunc
		get      http.HandlerFunc
	}{
		{
			desc: "no file and 404",
			head: statusHandler(http.StatusNotFound),
			get:  statusHandler(http.StatusNotFound),
		},
		{
			desc: "no file and connection reset before headers",
			head: headOK(len(payload), true),
			get:  resetConn,
		},
		{
			// Without Accept-Ranges, restart() truncates the partial file
			// before the GET fails.
			desc:     "partial file truncated by restart then 503",
			existing: payload[:400],
			head:     headOK(len(payload), false),
			get:      statusHandler(http.StatusServiceUnavailable),
		},
		{
			// A full-size file with the wrong checksum is
			// truncated before the GET fails.
			desc:     "full-size corrupt file then 404",
			existing: bytes.Repeat([]byte("x"), len(payload)),
			head:     headOK(len(payload), true),
			get:      statusHandler(http.StatusNotFound),
		},
		{
			// An empty file left by an older googet.
			desc:     "empty file left behind then 404",
			existing: []byte{},
			head:     statusHandler(http.StatusNotFound),
			get:      statusHandler(http.StatusNotFound),
		},
		{
			desc:     "empty file left behind then connection reset",
			existing: []byte{},
			head:     headOK(len(payload), true),
			get:      resetConn,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			srv := newMethodServer(t, tc.head, tc.get)
			dst := filepath.Join(t.TempDir(), "pkg.goo")
			if tc.existing != nil {
				if err := os.WriteFile(dst, tc.existing, 0644); err != nil {
					t.Fatalf("writing existing file: %v", err)
				}
			}
			if err := Package(context.Background(), srv.URL+"/pkg.goo", dst, chksum, downloader); err == nil {
				t.Fatal("Package() = nil, want an error")
			}
			if srv.count(http.MethodGet) == 0 {
				t.Errorf("server got no GET request, want at least 1")
			}
			assertNoFile(t, dst)
		})
	}
}

func TestLatest(t *testing.T) {
	// Each package payload names its own arch so the test can tell which
	// spec was downloaded.
	payloads := map[string][]byte{
		"/pkgs/foo.x86_64.goo": []byte("foo x86_64"),
		"/pkgs/foo.arm64.goo":  []byte("foo arm64"),
		"/pkgs/bar.x86_64.goo": []byte("bar x86_64"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, ok := payloads[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if r.Method == http.MethodGet {
			w.Write(payload)
		}
	}))
	defer srv.Close()

	repoSpec := func(name, arch string) goolib.RepoSpec {
		return goolib.RepoSpec{
			Checksum:    goolib.Checksum(bytes.NewReader(payloads["/pkgs/"+name+"."+arch+".goo"])),
			Source:      "pkgs/" + name + "." + arch + ".goo",
			PackageSpec: &goolib.PkgSpec{Name: name, Version: "1.0.0@1", Arch: arch},
		}
	}
	rm := client.RepoMap{
		srv.URL + "/repo": client.Repo{
			Packages: []goolib.RepoSpec{
				repoSpec("foo", "arm64"),
				repoSpec("foo", "x86_64"),
				repoSpec("bar", "x86_64"),
			},
		},
	}
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	hostArchs := []string{"noarch", "x86_64"}

	for _, tc := range []struct {
		desc     string
		pi       goolib.PackageInfo
		wantArch string // Empty means an error is expected.
	}{
		{
			desc:     "requested arch differs from host",
			pi:       goolib.PackageInfo{Name: "foo", Arch: "arm64"},
			wantArch: "arm64",
		},
		{
			desc:     "requested arch matches host",
			pi:       goolib.PackageInfo{Name: "foo", Arch: "x86_64"},
			wantArch: "x86_64",
		},
		{
			desc:     "no arch uses host preference",
			pi:       goolib.PackageInfo{Name: "foo"},
			wantArch: "x86_64",
		},
		{
			desc: "requested arch unavailable",
			pi:   goolib.PackageInfo{Name: "bar", Arch: "arm64"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dst, _, err := Latest(context.Background(), tc.pi, t.TempDir(), rm, hostArchs, downloader)
			if tc.wantArch == "" {
				if err == nil {
					t.Fatalf("Latest(%+v) = %q, nil; want error", tc.pi, dst)
				}
				return
			}
			if err != nil {
				t.Fatalf("Latest(%+v) = %v, want nil", tc.pi, err)
			}
			wantBase := goolib.PackageInfo{Name: tc.pi.Name, Arch: tc.wantArch, Ver: "1.0.0@1"}.PkgName()
			if got := filepath.Base(dst); got != wantBase {
				t.Errorf("Latest(%+v) saved %q, want %q", tc.pi, got, wantBase)
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("reading downloaded file: %v", err)
			}
			if want := payloads["/pkgs/"+tc.pi.Name+"."+tc.wantArch+".goo"]; !bytes.Equal(got, want) {
				t.Errorf("Latest(%+v) downloaded %q, want %q", tc.pi, got, want)
			}
		})
	}
}

// step scripts one GET response of stepServer. The zero value sends the
// whole requested range.
type step struct {
	hang    bool          // Stall before sending headers.
	status  int           // If nonzero, send only this status.
	stall   int           // If nonzero, stall after sending this many bytes.
	trickle time.Duration // If nonzero, send 50-byte chunks this far apart.
}

// stepServer serves payload with range support, answering the i-th GET as
// steps[i] describes and every later GET in full. It records the Range
// header of every GET in ranges, which is safe to read once the server is
// closed.
func stepServer(payload []byte, steps []step, ranges *[]string) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			return
		}
		mu.Lock()
		var s step
		if i := len(*ranges); i < len(steps) {
			s = steps[i]
		}
		*ranges = append(*ranges, r.Header.Get("Range"))
		mu.Unlock()
		if s.hang {
			<-r.Context().Done()
			return
		}
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		start, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-"))
		body := payload[start:]
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		switch {
		case s.trickle != 0:
			for chunk := range slices.Chunk(body, 50) {
				w.Write(chunk)
				w.(http.Flusher).Flush()
				time.Sleep(s.trickle)
			}
		case s.stall != 0:
			w.Write(body[:s.stall])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			w.Write(body)
		}
	}))
}

// stallDownloader returns a Downloader whose response headers and bodies
// stall after timeout, without changing the transport of http.DefaultClient.
func stallDownloader(timeout time.Duration) *client.Downloader {
	return &client.Downloader{
		HTTPClient:   &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: timeout}},
		StallTimeout: timeout,
	}
}

// setRetryDelay replaces retryDelay for the rest of the test.
func setRetryDelay(t *testing.T, d func(int) time.Duration) {
	t.Helper()
	old := retryDelay
	retryDelay = d
	t.Cleanup(func() { retryDelay = old })
}

func TestPackageRetries(t *testing.T) {
	if d1, d2 := retryDelay(1), retryDelay(2); d1 != time.Second || d2 != 2*time.Second {
		t.Fatalf("retryDelay(1), retryDelay(2) = %v, %v; want 1s, 2s", d1, d2)
	}
	setRetryDelay(t, func(int) time.Duration { return 0 })
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	const timeout = 400 * time.Millisecond
	downloader := stallDownloader(timeout)

	for _, tc := range []struct {
		desc       string
		steps      []step
		wantRanges []string
		wantErr    bool
	}{
		{
			desc:       "stall mid-body resumes",
			steps:      []step{{stall: 400}},
			wantRanges: []string{"bytes=0-", "bytes=400-"},
		},
		{
			desc:       "stall before headers is retried",
			steps:      []step{{hang: true}},
			wantRanges: []string{"bytes=0-", "bytes=0-"},
		},
		{
			// The body takes 2x the timeout but never pauses for long.
			desc:       "slow but progressing body is not interrupted",
			steps:      []step{{trickle: timeout / 10}},
			wantRanges: []string{"bytes=0-"},
		},
		{
			desc:       "server error is retried until maxAttempts",
			steps:      slices.Repeat([]step{{status: http.StatusServiceUnavailable}}, maxAttempts),
			wantRanges: slices.Repeat([]string{"bytes=0-"}, maxAttempts),
			wantErr:    true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var ranges []string
			srv := stepServer(payload, tc.steps, &ranges)
			dst := filepath.Join(t.TempDir(), "pkg.goo")
			err := Package(context.Background(), srv.URL+"/pkg.goo", dst, chksum, downloader)
			srv.Close()
			switch {
			case tc.wantErr:
				if err == nil {
					t.Error("Package() = nil, want an error")
				}
			case err != nil:
				t.Fatalf("Package() = %v, want nil", err)
			default:
				got, err := os.ReadFile(dst)
				if err != nil {
					t.Fatalf("reading downloaded file: %v", err)
				}
				if !bytes.Equal(got, payload) {
					t.Errorf("downloaded %d bytes, want %d bytes matching the payload", len(got), len(payload))
				}
			}
			if !slices.Equal(ranges, tc.wantRanges) {
				t.Errorf("GET ranges = %q, want %q", ranges, tc.wantRanges)
			}
		})
	}
}

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// TestIsPermanent checks that only 4xx statuses other than 408, 425 and 429,
// and a missing GCS object, are permanent.
func TestIsPermanent(t *testing.T) {
	status := func(code int) error {
		return &statusError{url: "http://example.com/pkg.goo", code: code, status: fmt.Sprintf("%d %s", code, http.StatusText(code))}
	}
	for _, tc := range []struct {
		desc string
		err  error
		want bool
	}{
		{"400", status(http.StatusBadRequest), true},
		{"401", status(http.StatusUnauthorized), true},
		{"403", status(http.StatusForbidden), true},
		{"404", status(http.StatusNotFound), true},
		{"410", status(http.StatusGone), true},
		{"wrapped 404", fmt.Errorf("attempt: %w", status(http.StatusNotFound)), true},
		{"304", status(http.StatusNotModified), false},
		{"408", status(http.StatusRequestTimeout), false},
		{"425", status(http.StatusTooEarly), false},
		{"429", status(http.StatusTooManyRequests), false},
		{"500", status(http.StatusInternalServerError), false},
		{"503", status(http.StatusServiceUnavailable), false},
		{"GCS object not found", storage.ErrObjectNotExist, true},
		{"net timeout", &url.Error{Op: "Get", URL: "http://example.com", Err: timeoutError{}}, false},
		{"stall", fmt.Errorf("downloading x: %w", client.ErrStalled), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"checksum mismatch", errors.New("checksum doesn't match: got a, want b"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if got := isPermanent(tc.err); got != tc.want {
				t.Errorf("isPermanent(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestPackageRetriesByStatus checks that Package stops after one attempt on a
// permanent GET status and retries the rest up to maxAttempts.
func TestPackageRetriesByStatus(t *testing.T) {
	setRetryDelay(t, func(int) time.Duration { return 0 })
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	for _, tc := range []struct {
		code     int
		wantGETs int
	}{
		{http.StatusNotFound, 1},
		{http.StatusTooManyRequests, maxAttempts},
		{http.StatusServiceUnavailable, maxAttempts},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			srv := newMethodServer(t, headOK(len(payload), true), statusHandler(tc.code))
			err := Package(context.Background(), srv.URL+"/pkg.goo", filepath.Join(t.TempDir(), "pkg.goo"), chksum, downloader)
			var sErr *statusError
			if !errors.As(err, &sErr) || sErr.code != tc.code {
				t.Errorf("Package() = %v, want a *statusError with code %d", err, tc.code)
			}
			if got := srv.count(http.MethodGet); got != tc.wantGETs {
				t.Errorf("GET requests = %d, want %d", got, tc.wantGETs)
			}
		})
	}

	// Only the GET status is classified, so a 404 HEAD followed by a good
	// GET succeeds on the first attempt.
	t.Run("HEAD 404 then GET 200", func(t *testing.T) {
		srv := newMethodServer(t, statusHandler(http.StatusNotFound), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Write(payload)
		})
		dst := filepath.Join(t.TempDir(), "pkg.goo")
		if err := Package(context.Background(), srv.URL+"/pkg.goo", dst, chksum, downloader); err != nil {
			t.Fatalf("Package() = %v, want nil", err)
		}
		if got := srv.count(http.MethodGet); got != 1 {
			t.Errorf("GET requests = %d, want 1", got)
		}
	})
}

// TestPackageRetriesChecksumMismatch checks that a checksum mismatch is retried
// and that the retry starts from an empty file.
func TestPackageRetriesChecksumMismatch(t *testing.T) {
	setRetryDelay(t, func(int) time.Duration { return 0 })
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "pkg.goo")
	var gets atomic.Int32
	var sizeAtRetry atomic.Int64
	srv := newMethodServer(t, headOK(len(payload), true), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if gets.Add(1) == 1 {
			w.Write(bytes.Repeat([]byte("x"), len(payload)))
			return
		}
		// packageHTTP has reopened dst by now; the corrupt bytes must be gone.
		if fi, err := os.Stat(dst); err == nil {
			sizeAtRetry.Store(fi.Size())
		}
		w.Write(payload)
	})
	if err := Package(context.Background(), srv.URL+"/pkg.goo", dst, chksum, downloader); err != nil {
		t.Fatalf("Package() = %v, want nil", err)
	}
	if got := srv.count(http.MethodGet); got != 2 {
		t.Errorf("GET requests = %d, want 2", got)
	}
	if got := sizeAtRetry.Load(); got != 0 {
		t.Errorf("dst had %d bytes when the retry started, want 0", got)
	}
	if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("downloaded file = %d bytes, %v; want the payload", len(got), err)
	}
}

func TestPackageGCSRetries(t *testing.T) {
	setRetryDelay(t, func(int) time.Duration { return 0 })
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if attempts.Add(1) == 1 {
			w.Write(payload[:200])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)

	dst := filepath.Join(t.TempDir(), "pkg.goo")
	if err := Package(context.Background(), "gs://bkt/pkg.goo", dst, chksum, stallDownloader(250*time.Millisecond)); err != nil {
		t.Fatalf("Package(gs://bkt/pkg.goo) = %v, want nil", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("downloaded %d bytes, want %d bytes matching the payload", len(got), len(payload))
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("GCS attempts = %d, want 2", got)
	}
}

// TestPackageGCSFailureLeavesNoFile checks end to end through the GCS emulator
// that dst doesn't exist after the last attempt fails.
func TestPackageGCSFailureLeavesNoFile(t *testing.T) {
	setRetryDelay(t, func(int) time.Duration { return 0 })
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	for _, tc := range []struct {
		desc    string
		handler http.HandlerFunc
	}{
		{
			desc: "checksum mismatch on every attempt",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				w.Write(bytes.Repeat([]byte("x"), len(payload)))
			},
		},
		{
			desc: "body cut short on every attempt",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				w.Write(payload[:400])
				w.(http.Flusher).Flush()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
			dst := filepath.Join(t.TempDir(), "pkg.goo")
			if err := Package(context.Background(), "gs://bkt/pkg.goo", dst, chksum, stallDownloader(2*time.Second)); err == nil {
				t.Fatal("Package(gs://bkt/pkg.goo) = nil, want an error")
			}
			assertNoFile(t, dst)
		})
	}
}

// TestPackageGCSDoesNotRetryNotFound checks that a missing GCS object is not
// retried.
func TestPackageGCSDoesNotRetryNotFound(t *testing.T) {
	setRetryDelay(t, func(int) time.Duration { return 0 })
	srv := newMethodServer(t, http.NotFound, http.NotFound)
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
	dst := filepath.Join(t.TempDir(), "pkg.goo")
	err := Package(context.Background(), "gs://bkt/pkg.goo", dst, "sum", stallDownloader(2*time.Second))
	if !client.IsGCSNotFound(err) {
		t.Errorf("Package(gs://bkt/pkg.goo) = %v, want an error that client.IsGCSNotFound accepts", err)
	}
	if got := srv.count(http.MethodGet) + srv.count(http.MethodHead); got != 1 {
		t.Errorf("GCS requests = %d, want 1", got)
	}
	assertNoFile(t, dst)
}

func TestPackageResumesAfterGivingUp(t *testing.T) {
	var gotRetries []int
	setRetryDelay(t, func(retry int) time.Duration {
		gotRetries = append(gotRetries, retry)
		return 0
	})
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	downloader := stallDownloader(200 * time.Millisecond)
	var ranges []string
	srv := stepServer(payload, slices.Repeat([]step{{stall: 100}}, maxAttempts), &ranges)
	dst := filepath.Join(t.TempDir(), "pkg.goo")
	err := Package(context.Background(), srv.URL+"/pkg.goo", dst, chksum, downloader)
	if !errors.Is(err, client.ErrStalled) {
		t.Errorf("first Package() = %v, want ErrStalled", err)
	}
	if !slices.Equal(gotRetries, []int{1, 2}) {
		t.Errorf("retryDelay calls = %v, want [1 2]", gotRetries)
	}
	// The failed download keeps the non-empty partial file (#216).
	if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, payload[:300]) {
		t.Fatalf("partial file after giving up = %d bytes, %v; want the first 300 payload bytes", len(got), err)
	}
	// A later run, such as the next googet invocation, resumes the partial
	// file.
	err = Package(context.Background(), srv.URL+"/pkg.goo", dst, chksum, downloader)
	srv.Close()
	if err != nil {
		t.Fatalf("second Package() = %v, want nil", err)
	}
	wantRanges := []string{"bytes=0-", "bytes=100-", "bytes=200-", "bytes=300-"}
	if !slices.Equal(ranges, wantRanges) {
		t.Errorf("GET ranges = %q, want %q", ranges, wantRanges)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("downloaded %d bytes, want %d bytes matching the payload", len(got), len(payload))
	}
}

func TestPackageStopsOnCancel(t *testing.T) {
	setRetryDelay(t, func(int) time.Duration {
		t.Error("Package() retried after its context was canceled")
		return 0
	})
	var ranges []string
	srv := stepServer(nil, []step{{hang: true}}, &ranges)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := Package(ctx, srv.URL+"/pkg.goo", filepath.Join(t.TempDir(), "pkg.goo"), "sum", stallDownloader(time.Hour))
	srv.Close()
	if !errors.Is(err, context.Canceled) || errors.Is(err, client.ErrStalled) {
		t.Errorf("Package() = %v, want context.Canceled and not ErrStalled", err)
	}
	if len(ranges) > 1 {
		t.Errorf("got %d GET requests, want at most 1", len(ranges))
	}
}

func TestExtractPkg(t *testing.T) {
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
