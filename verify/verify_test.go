/*
Copyright 2018 Google Inc. All Rights Reserved.
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
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

func TestMain(m *testing.M) {
	// Initialize the logger.
	logger.Init("test", false, false, ioutil.Discard)
	os.Exit(m.Run())
}

func TestFiles(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	testFile := filepath.Join(tempDir, "test")
	f, err := os.Create(testFile)
	if err != nil {
		t.Fatalf("error creating temp file: %v", err)
	}
	chksm := goolib.Checksum(f)
	if err := f.Close(); err != nil {
		t.Fatalf("error saving temp file: %v", err)
	}

	table := []struct {
		testCase string
		ps       client.PackageState
		verify   bool
	}{
		{"no files at all", client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, true},
		{"no files found", client.PackageState{InstalledFiles: map[string]string{"foo": "bar"}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, false},
		{"file checksum does not match", client.PackageState{InstalledFiles: map[string]string{testFile: "bar"}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, false},
		{"file checksum matches", client.PackageState{InstalledFiles: map[string]string{testFile: chksm}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, true},
		{"should skip folder", client.PackageState{InstalledFiles: map[string]string{tempDir: "", testFile: chksm}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, true},
	}
	for _, tt := range table {
		verify, err := Files(tt.ps)
		if err != nil {
			t.Errorf("%q: error running verify: %v", tt.testCase, err)
		}
		if verify != tt.verify {
			t.Errorf("%q: unexpected verification result, want: %t, got: %t", tt.testCase, tt.verify, verify)
		}
	}
}

// openHandles returns how many of this process's file descriptors refer to
// path, and false when /proc/self/fd isn't available to count them.
func openHandles(path string) (int, bool) {
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	n := 0
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && target == path {
			n++
		}
	}
	return n, true
}

func TestFilesReleasesDirectoryAndFileHandles(t *testing.T) {
	// openHandles compares against /proc/self/fd link targets, which are fully
	// resolved, so resolve symlinks in the temporary directory's path too.
	tempDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tempDir, "dir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("contents"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	chksm := goolib.Checksum(f)
	f.Close()

	ps := client.PackageState{
		InstalledFiles: map[string]string{dir: "", file: chksm},
		PackageSpec:    &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"},
	}
	ok, err := Files(ps)
	if err != nil || !ok {
		t.Fatalf("Files(%v) = %t, %v; want true, nil", ps.InstalledFiles, ok, err)
	}
	for _, p := range []string{dir, file} {
		if n, ok := openHandles(p); ok && n != 0 {
			t.Errorf("%d open handles to %q after Files, want 0", n, p)
		}
	}
	// Windows refuses to remove a file or directory while a handle to it is
	// open.
	for _, p := range []string{file, dir} {
		if err := os.Remove(p); err != nil {
			t.Errorf("os.Remove(%q) after Files: %v", p, err)
		}
	}
}
