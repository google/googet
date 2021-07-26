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
)

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
		{"should skip folder", client.PackageState{InstalledFiles: map[string]string{tempDir: "",testFile: chksm}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, true},
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
