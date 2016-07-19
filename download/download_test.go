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
	"io/ioutil"
	"path"
	"path/filepath"
	"testing"

	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
)

func init() {
	logger.Init("test", true, false, ioutil.Discard)
}

func TestDownload(t *testing.T) {
	r := bytes.NewReader([]byte("some content"))
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	chksum := goolib.Checksum(r)
	if _, err := r.Seek(0, 0); err != nil {
		t.Errorf("error seeking to front of reader: %v", err)
	}
	tempFile := path.Join(tempDir, "test")
	if err := download(r, tempFile, chksum, ""); err != nil {
		t.Errorf("error downloading and checking checksum: %v", err)
	}
	if err := download(r, tempFile, "notachecksum", ""); err == nil {
		t.Error("wanted but did not recieve checksum error")
	}
}

func TestExtractPkg(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
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

	name := "test"
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

	cts, err := ioutil.ReadFile(filepath.Join(dst, name))
	if err != nil {
		t.Fatalf("error opening test file: %v", err)
	}
	if string(cts) != body {
		t.Errorf("contents of extracted file does not match expected contents: got: %q, want: %q", string(cts), body)
	}
}
