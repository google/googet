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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// rangeServer serves payload, advertising range support on HEAD. When
// honorRange is false it answers ranged GETs with the whole file and 200 OK,
// as some proxies do.
func rangeServer(t *testing.T, payload []byte, honorRange bool, requests *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.Method+" "+r.Header.Get("Range"))
		w.Header().Set("Accept-Ranges", "bytes")
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
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[start:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Write(payload)
	}))
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
			desc:     "already downloaded",
			existing: payload,
			wantGETs: nil,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var requests []string
			srv := rangeServer(t, payload, tc.honorRange, &requests)
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
	hang   bool // Stall before sending headers.
	status int  // If nonzero, send only this status.
	stall  bool // Stall after sending send bytes of the range.
	send   int
}

// stepServer serves payload with range support, answering the i-th GET as
// steps[i] describes and every later GET in full. Every HEAD and GET waits
// delay before sending headers. It records the Range header of every GET in
// ranges, which is safe to read once the server is closed.
func stepServer(payload []byte, steps []step, delay time.Duration, ranges *[]string) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
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
		if !s.stall {
			w.Write(body)
			return
		}
		w.Write(body[:s.send])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
}

func TestPackageRetries(t *testing.T) {
	defer func(d func(int) time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = func(int) time.Duration { return 0 }
	payload := bytes.Repeat([]byte("0123456789"), 100)
	chksum := goolib.Checksum(bytes.NewReader(payload))
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	const timeout = 200 * time.Millisecond
	downloader.StallTimeout = timeout

	// Each attempt stalls after 100 more bytes, more times than maxFailures.
	var progressing []step
	var progressingRanges []string
	for i := range maxFailures + 2 {
		progressing = append(progressing, step{stall: true, send: 100})
		progressingRanges = append(progressingRanges, "bytes="+strconv.Itoa(100*i)+"-")
	}
	progressingRanges = append(progressingRanges, "bytes="+strconv.Itoa(100*(maxFailures+2))+"-")
	// Every attempt stalls without sending anything.
	var stuck []step
	var stuckRanges []string
	for range maxFailures {
		stuck = append(stuck, step{stall: true})
		stuckRanges = append(stuckRanges, "bytes=0-")
	}

	for _, tc := range []struct {
		desc       string
		existing   []byte
		chksum     string
		delay      time.Duration
		steps      []step
		wantRanges []string
		wantErr    error
		wantStatus int
	}{
		{
			desc:       "stall mid-body resumes",
			steps:      []step{{stall: true, send: 400}},
			wantRanges: []string{"bytes=0-", "bytes=400-"},
		},
		{
			desc:       "stall before headers is retried",
			steps:      []step{{hang: true}},
			wantRanges: []string{"bytes=0-", "bytes=0-"},
		},
		{
			// The HEAD and the GET headers each take most of the timeout,
			// so the timer must restart when each of them completes.
			desc:       "slow headers are progress",
			delay:      timeout * 3 / 5,
			wantRanges: []string{"bytes=0-"},
		},
		{
			desc:       "stalls that make progress never give up",
			steps:      progressing,
			wantRanges: progressingRanges,
		},
		{
			desc:       "gives up after repeated stalls without progress",
			steps:      append(stuck, step{stall: true}),
			wantRanges: stuckRanges,
			wantErr:    client.ErrStalled,
		},
		{
			desc:       "temporary server errors are retried",
			steps:      []step{{status: http.StatusServiceUnavailable}, {status: http.StatusTooManyRequests}, {status: http.StatusRequestTimeout}},
			wantRanges: []string{"bytes=0-", "bytes=0-", "bytes=0-", "bytes=0-"},
		},
		{
			desc:       "not found is not retried",
			steps:      []step{{status: http.StatusNotFound}},
			wantRanges: []string{"bytes=0-"},
			wantStatus: http.StatusNotFound,
		},
		{
			desc:       "checksum mismatch is not retried",
			chksum:     "bad",
			wantRanges: []string{"bytes=0-"},
			wantErr:    errChecksum,
		},
		{
			desc:       "checksum mismatch after resume restarts once",
			existing:   bytes.Repeat([]byte("x"), 400),
			wantRanges: []string{"bytes=400-", "bytes=0-"},
		},
		{
			desc:       "checksum mismatch after resume and restart gives up",
			existing:   bytes.Repeat([]byte("x"), 400),
			chksum:     "bad",
			wantRanges: []string{"bytes=400-", "bytes=0-"},
			wantErr:    errChecksum,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var ranges []string
			srv := stepServer(payload, tc.steps, tc.delay, &ranges)
			dst := filepath.Join(t.TempDir(), "pkg.goo")
			if tc.existing != nil {
				if err := os.WriteFile(dst, tc.existing, 0644); err != nil {
					t.Fatalf("writing existing file: %v", err)
				}
			}
			want := chksum
			if tc.chksum != "" {
				want = tc.chksum
			}
			err := Package(context.Background(), srv.URL+"/pkg.goo", dst, want, downloader)
			srv.Close()
			var se *statusError
			switch {
			case tc.wantStatus != 0:
				if !errors.As(err, &se) || se.code != tc.wantStatus {
					t.Errorf("Package() = %v, want status %d", err, tc.wantStatus)
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("Package() = %v, want %v", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("Package() = %v, want nil", err)
			default:
				if got, _ := os.ReadFile(dst); !bytes.Equal(got, payload) {
					t.Errorf("downloaded %d bytes, want %d bytes matching the payload", len(got), len(payload))
				}
			}
			if !slices.Equal(ranges, tc.wantRanges) {
				t.Errorf("GET ranges = %q, want %q", ranges, tc.wantRanges)
			}
		})
	}
}

func TestPackageDoesNotRetryPermanentErrors(t *testing.T) {
	defer func(d func(int) time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = func(int) time.Duration {
		t.Error("Package() retried a permanent error")
		return 0
	}
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		desc, url, dst string
	}{
		{desc: "unsupported scheme", url: "ftp://example.com/pkg.goo", dst: filepath.Join(dir, "pkg.goo")},
		{desc: "invalid URL", url: "http://[::1", dst: filepath.Join(dir, "pkg.goo")},
		{desc: "unwritable destination", url: "http://127.0.0.1:1/pkg.goo", dst: filepath.Join(dir, "missing", "pkg.goo")},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if err := Package(context.Background(), tc.url, tc.dst, "sum", downloader); err == nil {
				t.Error("Package() = nil, want an error")
			}
		})
	}
}

func TestPackageStopsOnCancel(t *testing.T) {
	var ranges []string
	srv := stepServer(nil, []step{{hang: true}}, 0, &ranges)
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err = Package(ctx, srv.URL+"/pkg.goo", filepath.Join(t.TempDir(), "pkg.goo"), "sum", downloader)
	srv.Close()
	if !errors.Is(err, context.Canceled) || errors.Is(err, client.ErrStalled) {
		t.Errorf("Package() = %v, want context.Canceled and not ErrStalled", err)
	}
	if len(ranges) != 1 {
		t.Errorf("got %d GET requests, want 1", len(ranges))
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
