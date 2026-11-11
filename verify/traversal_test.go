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
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	body := []byte("exit 0")
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.ps1", Mode: 0755, Size: int64(len(body))}); err != nil {
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

	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := extractVerify(&buf, "../evil.ps1", dir); err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("extractVerify error = %v, want a path traversal error", err)
	}
	if _, err := os.Stat(filepath.Join(root, "evil.ps1")); !os.IsNotExist(err) {
		t.Errorf("extractVerify wrote outside its directory: stat error = %v", err)
	}
}
