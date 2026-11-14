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
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

func TestScriptPathsOutsideDir(t *testing.T) {
	// Specs from a repo index or the database are not normalized, so a script
	// path can contain "..". Nothing may be written or run outside dir.
	for _, tc := range []struct {
		path          string
		wantTraversal bool
	}{
		{"../evil.cmd", true},
		// These name dir itself. On Windows the log is "<path>.log", so a
		// missing log-path check would write "pkg.log" next to dir. Elsewhere
		// they fail because a directory can't be run.
		{".", false},
		{"../pkg", false},
	} {
		for _, op := range []struct {
			name string
			run  func(dir, path string) error
		}{
			{"install", func(dir, path string) error {
				return Install(dir, &goolib.PkgSpec{Install: goolib.ExecFile{Path: path}})
			}},
			{"uninstall", func(dir, path string) error {
				return Uninstall(dir, &client.PackageState{PackageSpec: &goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: path}}})
			}},
			{"verify", func(dir, path string) error {
				return Verify(dir, &goolib.PkgSpec{Verify: goolib.ExecFile{Path: path}})
			}},
		} {
			t.Run(op.name+"/"+tc.path, func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, "pkg")
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				err := op.run(dir, tc.path)
				if err == nil {
					t.Fatal("succeeded, want error")
				}
				if tc.wantTraversal && !strings.Contains(err.Error(), "path traversal") {
					t.Errorf("error = %v, want a path traversal error", err)
				}
				entries, err := os.ReadDir(root)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 {
					t.Errorf("files next to dir: %v, want only pkg", entries)
				}
			})
		}
	}
}

func TestMsixUninstallFallbackOutsideDir(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the msix uninstall fallback is Windows only")
	}
	// With no Uninstall.Path, an msix package is removed using its
	// Install.Path, which comes from the spec and must stay inside dir.
	for _, path := range []string{`..\evil.msix`, `..\evil.msixbundle`} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "pkg")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			err := Uninstall(dir, &client.PackageState{PackageSpec: &goolib.PkgSpec{Install: goolib.ExecFile{Path: path}}})
			if err == nil || !strings.Contains(err.Error(), "path traversal") {
				t.Fatalf("Uninstall error = %v, want a path traversal error", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("files next to dir: %v, want only pkg", entries)
			}
		})
	}
}
