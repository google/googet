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

package remove

import (
	"io/ioutil"
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

func TestUninstallPkg(t *testing.T) {
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

	testFolder := filepath.Join(dst, "and")
	testFolder2 := filepath.Join(testFolder, "another")
	testFolder3 := filepath.Join(testFolder2, "level")
	if err := oswrap.MkdirAll(testFolder3, 0755); err != nil {
		t.Fatalf("Failed to create test folder: %v", err)
	}

	testFile := filepath.Join(testFolder3, "foo")
	if err := ioutil.WriteFile(testFile, []byte{}, 0666); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	st := &client.GooGetState{
		client.PackageState{
			PackageSpec: &goolib.PkgSpec{
				Name: "foo",
			},
			InstalledFiles: map[string]string{
				testFile:    "chksum",
				testFolder:  "",
				testFolder2: "",
				testFolder3: "",
				dst:         "",
			},
			UnpackDir: dst,
		},
	}

	if err := uninstallPkg(goolib.PackageInfo{Name: "foo"}, st, false, ""); err != nil {
		t.Fatalf("Error running uninstallPkg: %v", err)
	}

	for _, n := range []string{testFile, dst} {
		if _, err := oswrap.Stat(n); err == nil {
			t.Errorf("%s was not removed", n)
		}
	}
}

func TestBuild(t *testing.T) {
	pkg1 := "foo_pkg"
	pkg2 := "bar_pkg"
	pkg3 := "baz_pkg"
	as := ".noarch"
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    pkg1,
				Version: "1.0.0@1",
				Arch:    "noarch",
				PkgDependencies: map[string]string{
					pkg3 + as: "1.0.0@1",
				},
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    pkg2,
				Version: "1.0.0@1",
				Arch:    "noarch",
				PkgDependencies: map[string]string{
					pkg3 + as: "1.0.0@1",
					pkg1 + as: "1.0.0@1",
				},
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    pkg3,
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
	}

	table := []struct {
		pkg  string
		want DepMap
	}{
		{pkg1, DepMap{pkg1 + as: []string{pkg2 + as}, pkg2 + as: nil}},
		{pkg2, DepMap{pkg2 + as: nil}},
		{pkg3, DepMap{pkg1 + as: []string{pkg2 + as}, pkg2 + as: nil, pkg3 + as: []string{pkg1 + as, pkg2 + as}}},
	}
	for _, tt := range table {
		deps := make(DepMap)
		deps.build(tt.pkg, "noarch", state)
		if !reflect.DeepEqual(deps, tt.want) {
			t.Errorf("returned dependancy map does not match expected one: got %v, want %v", deps, tt.want)
		}
	}
}

func TestRemoveDep(t *testing.T) {
	pkg1 := "foo_pkg"
	pkg2 := "bar_pkg"
	pkg3 := "baz_pkg"
	deps := DepMap{pkg1: []string{pkg2}, pkg2: nil, pkg3: []string{pkg1, pkg2}}
	want := DepMap{pkg1: []string{}, pkg3: []string{pkg1}}
	deps.remove(pkg2)

	if !reflect.DeepEqual(deps, want) {
		t.Errorf("returned dependancy map does not match expected one: got %v, want %v", deps, want)
	}
}
