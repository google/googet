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
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/testutil"
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

func TestExtractVerify(t *testing.T) {
	files := []testutil.File{
		{Name: "payload.txt", Mode: 0644, Body: []byte("payload")},
		{Name: "verify.ps1", Mode: 0750, Body: []byte("exit 0")},
	}
	for _, format := range goolib.CompressionFormats {
		t.Run(format, func(t *testing.T) {
			var pkg bytes.Buffer
			testutil.WritePackage(t, &pkg, format, files)

			dir := filepath.Join(t.TempDir(), "verify")
			if err := extractVerify(bytes.NewReader(pkg.Bytes()), "verify.ps1", dir); err != nil {
				t.Fatalf("extractVerify: %v", err)
			}
			path := filepath.Join(dir, "verify.ps1")
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "exit 0" {
				t.Errorf("verify file contents = %q, want %q", got, "exit 0")
			}
			// Windows does not keep Unix permission bits.
			if runtime.GOOS != "windows" {
				fi, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm() != 0750 {
					t.Errorf("verify file mode = %v, want %v", fi.Mode().Perm(), os.FileMode(0750))
				}
			}

			err = extractVerify(bytes.NewReader(pkg.Bytes()), "missing.ps1", t.TempDir())
			want := `verify command "missing.ps1" not found in package`
			if err == nil || err.Error() != want {
				t.Errorf("extractVerify with missing command: error = %v, want %q", err, want)
			}
		})
	}
}

func TestCommandCachedPackage(t *testing.T) {
	// The verify script must be runnable by goolib.Exec on the test OS.
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skipf("goolib.Exec does not support %s", runtime.GOOS)
	}
	script, body := "verify.sh", "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		script, body = "verify.cmd", "@exit /b 0\r\n"
	}
	for _, format := range goolib.CompressionFormats {
		t.Run(format, func(t *testing.T) {
			var pkg bytes.Buffer
			testutil.WritePackage(t, &pkg, format, []testutil.File{
				{Name: script, Mode: 0755, Body: []byte(body)},
			})
			local := filepath.Join(t.TempDir(), "foo.noarch.1.0.0@1.goo")
			if err := os.WriteFile(local, pkg.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
			// A cached package whose checksum matches is verified from the local
			// file without downloading, so no downloader is needed.
			ps := client.PackageState{
				LocalPath: local,
				Checksum:  goolib.Checksum(bytes.NewReader(pkg.Bytes())),
				PackageSpec: &goolib.PkgSpec{
					Name:    "foo",
					Arch:    "noarch",
					Version: "1.0.0@1",
					Verify:  goolib.ExecFile{Path: script},
				},
			}
			ok, err := Command(context.Background(), ps, nil)
			if err != nil {
				t.Fatalf("Command: %v", err)
			}
			if !ok {
				t.Error("Command = false, want true")
			}
		})
	}
}
