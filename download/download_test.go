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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

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
