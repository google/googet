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

package verify

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractVerifyOutsideDir(t *testing.T) {
	body := []byte("exit 0")
	buf := gooArchive(t, "../evil.ps1", body)

	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := extractVerify(buf, "../evil.ps1", dir); err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("extractVerify error = %v, want a path traversal error", err)
	}
	if _, err := os.Stat(filepath.Join(root, "evil.ps1")); !os.IsNotExist(err) {
		t.Errorf("extractVerify wrote outside its directory: stat error = %v", err)
	}
}

func TestExtractVerifySubdirectory(t *testing.T) {
	body := []byte("exit 0")
	buf := gooArchive(t, "scripts/verify.ps1", body)

	dir := filepath.Join(t.TempDir(), "pkg")
	if err := extractVerify(buf, "scripts/verify.ps1", dir); err != nil {
		t.Fatalf("extractVerify: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "scripts", "verify.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("verify script = %q, want %q", got, body)
	}
}

// gooArchive returns a gzip-compressed tar archive holding one file.
func gooArchive(t *testing.T, name string, body []byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}
