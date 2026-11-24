//go:build !windows

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
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestPrepareRootNonWindows(t *testing.T) {
	tmp := t.TempDir()
	ref := filepath.Join(tmp, "ref")
	if err := os.MkdirAll(ref, 0774); err != nil {
		t.Fatalf("creating reference directory: %v", err)
	}
	refInfo, err := os.Stat(ref)
	if err != nil {
		t.Fatalf("stating reference directory: %v", err)
	}

	root := filepath.Join(tmp, "a", "b", "googet")
	r, err := PrepareRoot(root, []string{"repos", "cache"})
	if err != nil {
		t.Fatalf("PrepareRoot(%q) failed: %v", root, err)
	}
	if diff := cmp.Diff(RootReport{}, r, cmp.AllowUnexported(RootReport{})); diff != "" {
		t.Errorf("PrepareRoot(%q) report mismatch (-want +got):\n%s", root, diff)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stating created root %q: %v", root, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%q is not a directory", root)
	}
	if fi.Mode().Perm() != refInfo.Mode().Perm() {
		t.Errorf("mode of %q = %o, want %o", root, fi.Mode().Perm(), refInfo.Mode().Perm())
	}

	// Calling PrepareRoot again on an existing root succeeds with no error.
	if r2, err := PrepareRoot(root, []string{"repos", "cache"}); err != nil {
		t.Fatalf("second PrepareRoot(%q) failed: %v", root, err)
	} else if diff := cmp.Diff(RootReport{}, r2, cmp.AllowUnexported(RootReport{})); diff != "" {
		t.Errorf("second PrepareRoot(%q) report mismatch (-want +got):\n%s", root, diff)
	}
}

func TestCheckEntriesNonWindows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	r, err := CheckEntries(root, testEntryRules, RootReport{})
	if err != nil {
		t.Fatalf("CheckEntries(%q) failed: %v", root, err)
	}
	if diff := cmp.Diff(EntryReport{}, r); diff != "" {
		t.Errorf("CheckEntries(%q) report mismatch (-want +got):\n%s", root, diff)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("CheckEntries(%q) created the missing root (Lstat error: %v)", root, err)
	}

	for _, bad := range []EntryRule{
		{Dir: "..", Pattern: "x", Action: EntryRemove},
		{Pattern: "[", Action: EntryRemove},
		{Pattern: "x"},
	} {
		if _, err := CheckEntries(root, []EntryRule{bad}, RootReport{}); err == nil {
			t.Errorf("CheckEntries with invalid rule %+v = nil, want an error", bad)
		}
	}
}
