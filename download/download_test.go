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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/testutil"
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
			desc:       "any error is retried until maxAttempts",
			steps:      slices.Repeat([]step{{status: http.StatusNotFound}}, maxAttempts),
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

// writePkg writes a package holding files, compressed with format, to a
// test.pkg file in a new temporary directory and returns its path.
func writePkg(t *testing.T, format string, files []testutil.File) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "test.pkg")
	f, err := oswrap.Create(src)
	if err != nil {
		t.Fatalf("error creating temp file: %v", err)
	}
	testutil.WritePackage(t, f, format, files)
	if err := f.Close(); err != nil {
		t.Fatalf("error closing file: %v", err)
	}
	return src
}

// extractedFile describes a file written by ExtractPkg.
type extractedFile struct {
	Body string
	Mode os.FileMode
}

// readTree returns the regular files under dir, keyed by slash-separated
// relative path.
func readTree(t *testing.T, dir string) map[string]extractedFile {
	t.Helper()
	got := make(map[string]extractedFile)
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = extractedFile{Body: string(b), Mode: fi.Mode()}
		return nil
	})
	if err != nil {
		t.Fatalf("error reading extracted files: %v", err)
	}
	return got
}

func TestExtractPkg(t *testing.T) {
	files := []testutil.File{
		{Name: "foo/../test", Mode: 0600, Body: []byte("this is a test file")},
		{Name: "dir/sub/tool.exe", Mode: 0755, Body: []byte("binary contents")},
		{Name: "empty", Mode: 0644},
	}
	wantBodies := map[string]string{
		"test":             "this is a test file",
		"dir/sub/tool.exe": "binary contents",
		"empty":            "",
	}
	trees := make(map[string]map[string]extractedFile)
	for _, format := range goolib.CompressionFormats() {
		t.Run(format, func(t *testing.T) {
			dst, err := ExtractPkg(writePkg(t, format, files))
			if err != nil {
				t.Fatalf("error running ExtractPkg: %v", err)
			}
			got := readTree(t, dst)
			gotBodies := make(map[string]string)
			for name, f := range got {
				gotBodies[name] = f.Body
			}
			if diff := cmp.Diff(wantBodies, gotBodies); diff != "" {
				t.Errorf("extracted files mismatch (-want +got):\n%s", diff)
			}
			trees[format] = got
		})
	}
	// File sets, contents and modes must not depend on the compression format.
	if t.Failed() {
		return
	}
	if diff := cmp.Diff(trees[goolib.CompressionGzip], trees[goolib.CompressionZstd]); diff != "" {
		t.Errorf("zstd extraction differs from gzip (-gzip +zstd):\n%s", diff)
	}
}

func TestExtractPkgPathTraversal(t *testing.T) {
	// The package extracts to <tmp>/test, so these names resolve outside it.
	// Avoid names like "foo/../../test" that resolve to the destination
	// directory itself and fail for an unrelated reason.
	for _, name := range []string{"foo/../../escaped", "../escaped"} {
		files := []testutil.File{{Name: name, Mode: 0600, Body: []byte("this is a test file")}}
		for _, format := range goolib.CompressionFormats() {
			t.Run(format+" "+name, func(t *testing.T) {
				src := writePkg(t, format, files)
				_, err := ExtractPkg(src)
				if err == nil || !strings.Contains(err.Error(), "path traversal") {
					t.Fatalf("ExtractPkg error = %v, want path traversal error", err)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(src), "escaped")); !os.IsNotExist(err) {
					t.Errorf("ExtractPkg wrote outside the destination directory: stat error = %v", err)
				}
			})
		}
	}
}

func TestExtractPkgNotCompressed(t *testing.T) {
	var rawTar bytes.Buffer
	tw := tar.NewWriter(&rawTar)
	body := "this is a test file"
	if err := tw.WriteHeader(&tar.Header{Name: "test", Mode: 0600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		content []byte
	}{
		{"uncompressed tar", rawTar.Bytes()},
		{"garbage", []byte("this is not a package")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "test.pkg")
			if err := os.WriteFile(src, tc.content, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ExtractPkg(src); !errors.Is(err, gzip.ErrHeader) {
				t.Errorf("ExtractPkg error = %v, want %v", err, gzip.ErrHeader)
			}
			if got := readTree(t, strings.TrimSuffix(src, ".pkg")); len(got) != 0 {
				t.Errorf("ExtractPkg extracted %v, want no files", got)
			}
		})
	}
}
