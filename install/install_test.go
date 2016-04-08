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

package install

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/googet/client"
	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
)

func init() {
	logger.Init("test", true, false, ioutil.Discard)
}

func TestMinInstalled(t *testing.T) {
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "foo_pkg",
				Version: "1.2.3@4",
				Arch:    "noarch",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "bar_pkg",
				Version: "0.1.0@1",
				Arch:    "noarch",
			},
		},
	}

	table := []struct {
		pkg, arch string
		ins       bool
	}{
		{"foo_pkg", "noarch", true},
		{"foo_pkg", "", true},
		{"foo_pkg", "x86_64", false},
		{"bar_pkg", "noarch", false},
		{"baz_pkg", "noarch", false},
	}
	for _, tt := range table {
		ma, err := minInstalled(goolib.PackageInfo{tt.pkg, tt.arch, "1.0.0@1"}, state)
		if err != nil {
			t.Fatalf("error checking minAvailable: %v", err)
		}
		if ma != tt.ins {
			t.Errorf("minInstalled returned %v for %q when it should return %v", ma, tt.pkg, tt.ins)
		}
	}
}

func TestNeedsInstallation(t *testing.T) {
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "foo_pkg",
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "bar_pkg",
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "baz_pkg",
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
	}

	table := []struct {
		pkg string
		ver string
		ins bool
	}{
		{"foo_pkg", "1.0.0@1", false}, // equal
		{"bar_pkg", "2.0.0@1", true},  // higher
		{"baz_pkg", "0.1.0@1", false}, // lower
		{"pkg", "1.0.0@1", true},      // not installed
	}
	for _, tt := range table {
		ins, err := NeedsInstallation(goolib.PackageInfo{tt.pkg, "noarch", tt.ver}, state)
		if err != nil {
			t.Fatalf("Error checking NeedsInstallation: %v", err)
		}
		if ins != tt.ins {
			t.Errorf("NeedsInstallation returned %v for %q when it should return %v", ins, tt.pkg, tt.ins)
		}
	}
}

func TestInstallPkg(t *testing.T) {
	src, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(src)

	dst, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	dst += ("/this/is/an/extremely/long/filename/you/wouldnt/expect/to/see/it/" +
		"in/the/wild/but/you/would/actually/be/surprised/at/some/of/the/" +
		"stuff/that/pops/up/and/seriously/two/hundred/and/fify/five/chars" +
		"is/quite/a/large/number/but/somehow/there/were/real/goo/packages" +
		"which/exceeded/this/limit/hence/this/absurdly/long/string/in/" +
		"this/unit/test")

	defer oswrap.RemoveAll(dst)

	files := []string{"test1", "test2", "test3"}
	want := map[string]string{dst: ""}
	for _, n := range files {
		f, err := oswrap.Create(filepath.Join(src, n))
		if err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
		want[filepath.Join(dst, n)] = goolib.Checksum(f)
		if err := f.Close(); err != nil {
			t.Fatalf("Failed to close test file: %v", err)
		}
	}

	ps := goolib.PkgSpec{Files: map[string]string{filepath.Base(src): dst}}

	got, err := installPkg(filepath.Dir(src), &ps, false)
	if err != nil {
		t.Fatalf("Error running installPkg: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("installPkg did not return expected file list, got: %+v, want: %+v", got, want)
	}

	for _, n := range files {
		want := filepath.Join(dst, n)
		if _, err := oswrap.Stat(want); err != nil {
			t.Errorf("Expected test file %s does not exist", want)
		}
	}
}

func TestCleanOldFiles(t *testing.T) {
	src, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(src)

	dst, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(dst)

	for _, n := range []string{filepath.Join(src, "test1"), filepath.Join(src, "test2")} {
		if err := ioutil.WriteFile(n, []byte{}, 0666); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
	}

	want := filepath.Join(dst, "test1")
	notWant := filepath.Join(dst, "test2")
	dontCare := filepath.Join(dst, "test3")
	for _, n := range []string{want, notWant, dontCare} {
		if err := ioutil.WriteFile(n, []byte{}, 0666); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
	}

	st := client.PackageState{
		PackageSpec: &goolib.PkgSpec{
			Files: map[string]string{filepath.Base(src): dst},
		},
		InstalledFiles: map[string]string{
			want:    "chksum",
			notWant: "chksum",
			dst:     "",
		},
	}

	cleanOldFiles(dst, st, map[string]string{want: "", dst: ""})

	for _, n := range []string{want, dontCare} {
		if _, err := oswrap.Stat(n); err != nil {
			t.Errorf("Expected test file %s does not exist", want)
		}
	}

	if _, err := oswrap.Stat(notWant); err == nil {
		t.Errorf("Deprecated file %s not removed", notWant)
	}
}

func TestResolveDst(t *testing.T) {
	if err := os.Setenv("foo", "bar"); err != nil {
		t.Errorf("error setting environment variable: %v", err)
	}

	table := []struct {
		dst, want string
	}{
		{"<foo>/some/place", "bar/some/place"},
		{"<foo/some/place", "/<foo/some/place"},
		{"something/<foo>/some/place", "/something/<foo>/some/place"},
	}
	for _, tt := range table {
		got := resolveDst(tt.dst)
		if got != tt.want {
			t.Errorf("resolveDst returned %s, want %s", got, tt.want)
		}
	}
}
