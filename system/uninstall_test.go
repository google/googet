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

package system

import (
	"os"
	"runtime"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

func TestUninstallNeedsPackage(t *testing.T) {
	// Only Windows removes an msix package using its install path, so only
	// Windows needs the package for it.
	msixNeedsPackage := runtime.GOOS == "windows"
	for _, tc := range []struct {
		name string
		ps   goolib.PkgSpec
		want bool
	}{
		{"files only", goolib.PkgSpec{}, false},
		{"uninstall script", goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}}, true},
		{"msix", goolib.PkgSpec{Install: goolib.ExecFile{Path: "app.msix"}}, msixNeedsPackage},
		{"msixbundle", goolib.PkgSpec{Install: goolib.ExecFile{Path: "app.msixbundle"}}, msixNeedsPackage},
		{"msi from registry", goolib.PkgSpec{Install: goolib.ExecFile{Path: "setup.msi"}}, false},
		{"exe from registry", goolib.PkgSpec{Install: goolib.ExecFile{Path: "setup.exe"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := UninstallNeedsPackage(&tc.ps); got != tc.want {
				t.Errorf("UninstallNeedsPackage(%+v) = %v, want %v", tc.ps, got, tc.want)
			}
		})
	}
}

// When UninstallNeedsPackage is false, Uninstall must not run anything from
// dir, so remove can pass it an empty directory. On Windows the msi and exe
// cases look up the registry instead, so this only runs elsewhere.
func TestUninstallWithoutPackageRunsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows looks up an uninstaller in the registry")
	}
	for _, tc := range []struct {
		name string
		ps   goolib.PkgSpec
	}{
		{"files only", goolib.PkgSpec{}},
		{"msix", goolib.PkgSpec{Install: goolib.ExecFile{Path: "app.msix"}}},
		{"msixbundle", goolib.PkgSpec{Install: goolib.ExecFile{Path: "app.msixbundle"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if UninstallNeedsPackage(&tc.ps) {
				t.Fatalf("UninstallNeedsPackage(%+v) = true, want false", tc.ps)
			}
			dir := t.TempDir()
			if err := Uninstall(dir, &client.PackageState{PackageSpec: &tc.ps}); err != nil {
				t.Fatalf("Uninstall: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("Uninstall wrote %v in dir, want nothing", entries)
			}
		})
	}
}
