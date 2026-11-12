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

package main

import (
	"bytes"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestThirdPartyLicensesCurrent runs update_third_party_licenses.sh into a
// temporary directory and compares the generated tree against the committed
// THIRD_PARTY_LICENSES directory, so CI fails whenever a dependency change
// leaves the shipped licence notices out of date.
func TestThirdPartyLicensesCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping third-party licence regeneration in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("update_third_party_licenses.sh runs in the Linux CI job")
	}
	// Bazel and Blaze run tests in a sandbox without the script, the licence
	// tree, a Go toolchain or network access, so the check only makes sense
	// in a plain "go test" run from a checkout.
	if os.Getenv("TEST_SRCDIR") != "" {
		t.Skip("skipping third-party licence regeneration in a Bazel/Blaze sandbox")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	goproxy, err := exec.Command("go", "env", "GOPROXY").Output()
	if err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}
	if string(bytes.TrimSpace(goproxy)) == "off" {
		t.Skip("skipping third-party licence regeneration when GOPROXY=off")
	}
	if _, err := os.Stat("update_third_party_licenses.sh"); err != nil {
		t.Fatalf("update_third_party_licenses.sh missing from checkout: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "THIRD_PARTY_LICENSES")
	cmd := exec.Command("bash", "./update_third_party_licenses.sh", outDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("update_third_party_licenses.sh failed: %v\n%s", err, out)
	}
	if len(bytes.TrimSpace(out)) > 0 {
		t.Logf("update_third_party_licenses.sh output:\n%s", out)
	}

	want := readLicenseTree(t, outDir)
	got := readLicenseTree(t, "THIRD_PARTY_LICENSES")

	wantPaths := slices.Sorted(maps.Keys(want))
	gotPaths := slices.Sorted(maps.Keys(got))
	if diff := cmp.Diff(wantPaths, gotPaths); diff != "" {
		t.Fatalf("THIRD_PARTY_LICENSES file list is out of date (-generated +committed); run ./update_third_party_licenses.sh:\n%s", diff)
	}
	for _, p := range wantPaths {
		if diff := cmp.Diff(string(want[p]), string(got[p])); diff != "" {
			t.Errorf("THIRD_PARTY_LICENSES/%s differs from generated output (-generated +committed); run ./update_third_party_licenses.sh:\n%s", p, diff)
		}
	}
}

func readLicenseTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	return files
}
