//go:build windows

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

package oswrap

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNormPath(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for _, tc := range []struct {
		path, want string
	}{
		{`C:\foo\bar`, `\\?\C:\foo\bar`},
		{`C:\foo\..\bar\.\baz`, `\\?\C:\bar\baz`},
		{`C:/foo/bar`, `\\?\C:\foo\bar`},
		{`rel\file`, `\\?\` + filepath.Join(cwd, `rel\file`)},
		{`\\server\share\x`, `\\?\UNC\server\share\x`},
		{`//server/share/x`, `\\?\UNC\server\share\x`},
		{`\\server\share\a\..\x`, `\\?\UNC\server\share\x`},
		{`\\?\C:\foo\..\bar`, `\\?\C:\foo\..\bar`},
		{`\\?\UNC\server\share\x`, `\\?\UNC\server\share\x`},
		{`\\.\pipe\name`, `\\.\pipe\name`},
	} {
		got, err := normPath(tc.path)
		if err != nil {
			t.Errorf("normPath(%q): %v", tc.path, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// writeTemp writes content to a new file in a temporary directory and returns
// its path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatalf("writing %q: %v", name, err)
	}
	return name
}

func TestOpenSharedAllowsRenameAndRemove(t *testing.T) {
	name := writeTemp(t, "content")
	f, err := OpenShared(name)
	if err != nil {
		t.Fatalf("OpenShared(%q): %v", name, err)
	}
	defer f.Close()

	renamed := name + ".new"
	if err := os.Rename(name, renamed); err != nil {
		t.Fatalf("os.Rename while the file is open with OpenShared: %v", err)
	}
	// The open handle keeps reading the renamed file.
	if b, err := io.ReadAll(f); err != nil || string(b) != "content" {
		t.Errorf("reading after the rename = %q, %v; want %q, nil", b, err, "content")
	}
	if err := os.Remove(renamed); err != nil {
		t.Errorf("os.Remove while the file is open with OpenShared: %v", err)
	}
}

func TestOsOpenBlocksRename(t *testing.T) {
	// The contrast with os.Open shows that the sharing in OpenShared is what
	// lets the rename through. This documents why OpenShared exists rather
	// than testing googet code, so it skips if a Go release makes os.Open
	// share delete access.
	name := writeTemp(t, "content")
	f, err := os.Open(name)
	if err != nil {
		t.Fatalf("os.Open(%q): %v", name, err)
	}
	defer f.Close()
	err = os.Rename(name, name+".new")
	if err == nil {
		t.Skip("os.Open shares delete access in this Go release, so OpenShared is no longer needed for it")
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Errorf("os.Rename while the file is open with os.Open = %v, want a sharing violation", err)
	}
}

func TestOpenSharedDirectory(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenShared(dir)
	if err == nil {
		f.Close()
		t.Fatalf("OpenShared(directory) = nil error, want an error")
	}
}

func TestOpenSharedLongPath(t *testing.T) {
	// Longer than MAX_PATH (260), which CreateFile only accepts with the
	// \\?\ prefix that normPath adds.
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 100), strings.Repeat("e", 100), strings.Repeat("f", 100))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating %q: %v", dir, err)
	}
	name := filepath.Join(dir, "file")
	if err := os.WriteFile(name, []byte("long"), 0644); err != nil {
		t.Fatalf("writing %q: %v", name, err)
	}
	b, err := ReadFileShared(name)
	if err != nil || string(b) != "long" {
		t.Errorf("ReadFileShared(%d-character path) = %q, %v; want %q, nil", len(name), b, err, "long")
	}
}
