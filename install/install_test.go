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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
)

func init() {
	logger.Init("test", true, false, ioutil.Discard)
}

func TestMinInstalled(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
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
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	db.WriteStateToDB(state)
	table := []struct {
		pkg, arch string
		ins       bool
	}{
		{"foo_pkg", "noarch", true},
		{"foo_pkg", "", true},
		{"foo_pkg", "x86_64", false},
		{"foo_pkg", "arm64", false},
		{"bar_pkg", "noarch", false},
		{"baz_pkg", "noarch", false},
	}
	for _, tt := range table {
		ma, err := minInstalled(goolib.PackageInfo{Name: tt.pkg, Arch: tt.arch, Ver: "1.0.0@1"}, db)
		if err != nil {
			t.Fatalf("error checking minAvailable: %v", err)
		}
		if ma != tt.ins {
			t.Errorf("minInstalled returned %v for %q when it should return %v", ma, tt.pkg, tt.ins)
		}
	}
}

func TestNeedsInstallation(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
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
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	db.WriteStateToDB(state)
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
		ins, err := NeedsInstallation(goolib.PackageInfo{Name: tt.pkg, Arch: "noarch", Ver: tt.ver}, db)
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

	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

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
	dst = filepath.FromSlash(dst)

	defer oswrap.RemoveAll(dst)

	f, err := os.Create(filepath.Join(src, "test.goo"))
	if err != nil {
		log.Fatal(err)
	}
	defer oswrap.Remove(f.Name())

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	files := []string{"test1", "test2", "test3"}
	want := map[string]string{dst: ""}
	for _, n := range files {
		f, err := oswrap.Create(filepath.Join(src, n))
		if err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		fi, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		fih, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(fih); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, f); err != nil {
			t.Fatal(err)
		}

		want[filepath.Join(dst, n)] = goolib.Checksum(f)
		if err := f.Close(); err != nil {
			t.Fatalf("Failed to close test file: %v", err)
		}
	}

	tw.Close()
	gw.Close()
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	ps := goolib.PkgSpec{Files: map[string]string{"./": dst}}
	got, err := installPkg(defaultInstallOps(), f.Name(), &ps, false, false, db)
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

	cleanOldFiles(st, map[string]string{want: "", dst: ""})

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

func TestIsSatisfied(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:     "provider_pkg",
				Version:  "1.0.0@1",
				Arch:     "noarch",
				Provides: []string{"libfoo", "libbar=1.5.0"},
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "real_pkg",
				Version: "2.0.0@1",
				Arch:    "noarch",
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	tests := []struct {
		name string
		pi   goolib.PackageInfo
		want bool
	}{
		{
			name: "Directly installed package",
			pi:   goolib.PackageInfo{Name: "real_pkg", Arch: "noarch", Ver: "1.0.0"},
			want: true,
		},
		{
			name: "Provided package without version",
			pi:   goolib.PackageInfo{Name: "libfoo", Arch: "noarch", Ver: "1.0.0"},
			want: true,
		},
		{
			name: "Provided package with satisfied version",
			pi:   goolib.PackageInfo{Name: "libbar", Arch: "noarch", Ver: "1.0.0"},
			want: true,
		},
		{
			name: "Provided package with unsatisfied version",
			pi:   goolib.PackageInfo{Name: "libbar", Arch: "noarch", Ver: "2.0.0"},
			want: false,
		},
		{
			name: "Not installed and not provided",
			pi:   goolib.PackageInfo{Name: "missing_pkg", Arch: "noarch", Ver: "1.0.0"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isSatisfied(tt.pi, db)
			if err != nil {
				t.Fatalf("isSatisfied error: %v", err)
			}
			if got != tt.want {
				t.Errorf("isSatisfied(%v) = %v, want %v", tt.pi, got, tt.want)
			}
		})
	}
}

func TestResolveConflicts_Provides(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:     "provider_pkg",
				Version:  "1.0.0@1",
				Arch:     "noarch",
				Provides: []string{"libconflict"},
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	ps := &goolib.PkgSpec{
		Name:      "conflicting_pkg",
		Version:   "1.0.0@1",
		Arch:      "noarch",
		Conflicts: []string{"libconflict"},
	}

	err = resolveConflicts(ps, db)
	if err == nil {
		t.Error("resolveConflicts expected error, got nil")
	} else {
		expectedErr := "cannot install, conflict with installed package or provider: libconflict"
		if err.Error() != expectedErr {
			t.Errorf("resolveConflicts error = %q, want %q", err.Error(), expectedErr)
		}
	}
}

func TestFromRepo_SatisfiedByProvider(t *testing.T) {
	// This is a more integration-level test to ensure installDeps uses isSatisfied.
	// We mock the DB state and call installDeps directly or via a wrapper if accessible.
	// installDeps is unexported, but we are in package install.

	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:     "provider_pkg",
				Version:  "1.0.0@1",
				Arch:     "noarch",
				Provides: []string{"libvirt"},
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	// Package wanting libvirt
	ps := &goolib.PkgSpec{
		Name:            "consumer_pkg",
		Version:         "1.0.0@1",
		Arch:            "noarch",
		PkgDependencies: map[string]string{"libvirt": "1.0.0"},
	}

	// We pass empty repo map and downloader because we expect it NOT to try downloading deps
	// since they are satisfied.
	err = installDeps(t.Context(), ps, "", nil, nil, false, false, nil, db)
	if err != nil {
		t.Errorf("installDeps failed: %v", err)
	}
}

func TestFromRepo_SatisfiedByUninstalledProvider(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Repo state with provider
	rm := client.RepoMap{
		"repo1": client.Repo{
			Priority: priority.Value(500),
			Packages: []goolib.RepoSpec{
				{
					PackageSpec: &goolib.PkgSpec{
						Name:     "provider_pkg",
						Version:  "1.0.0@1",
						Arch:     "noarch",
						Provides: []string{"libvirt"},
					},
				},
			},
		},
	}

	// Package wanting libvirt
	ps := &goolib.PkgSpec{
		Name:            "consumer_pkg",
		Version:         "1.0.0@1",
		Arch:            "noarch",
		PkgDependencies: map[string]string{"libvirt": "1.0.0"},
	}

	// Verify that dependency resolution succeeds (finding provider_pkg); the download
	// is expected to fail due to an invalid repository URL.
	downloader, _ := client.NewDownloader("")
	err = installDeps(t.Context(), ps, "", rm, []string{"noarch"}, false, false, downloader, db)

	// We expect an error because download will fail (invalid URL/Source).
	if err == nil {
		t.Error("installDeps expected error, got nil")
	} else {
		// Verify that the error is not a resolution error.
		// Any other error implies resolution succeeded and it failed at the download stage.
		errMsg := err.Error()
		if errMsg == "cannot resolve dependency, libvirt.noarch version 1.0.0 or greater not installed and not available in any repo" {
			t.Errorf("installDeps failed to resolve provider: %v", err)
		}
		// Any other error means it TRIED to install it (provider found).
		t.Logf("Got expected error (confirming resolution success): %v", err)
	}
}

func TestBuildConflictMap(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgA", Version: "1.0.0@1", Arch: "noarch"},
			InstalledFiles: map[string]string{
				"/path/to/file1": "chksum1",
				"/path/to/dir1":  "",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgB", Version: "1.0.0@1", Arch: "noarch"},
			InstalledFiles: map[string]string{
				"/path/to/file2": "chksum2",
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	cm, err := buildConflictMap(db, "pkgB")
	if err != nil {
		t.Fatalf("buildConflictMap: %v", err)
	}

	if _, ok := cm["/path/to/dir1"]; ok {
		t.Errorf("buildConflictMap included directory /path/to/dir1")
	}
	if owner, ok := cm["/path/to/file1"]; !ok || owner != "pkgA" {
		t.Errorf("expected /path/to/file1 to map to pkgA, got %v", owner)
	}
	if _, ok := cm["/path/to/file2"]; ok {
		t.Errorf("buildConflictMap included file from excluded package pkgB")
	}
}

func TestMakeInstallFunction(t *testing.T) {
	dstDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer oswrap.RemoveAll(dstDir)

	srcDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	srcDir = filepath.Join(srcDir, "foo") // append subdirectory to properly test TrimPrefix
	oswrap.MkdirAll(srcDir, 0755)
	defer oswrap.RemoveAll(srcDir)

	conflictPath := filepath.Join(dstDir, "conflicting_file")
	cm := map[string]string{
		conflictPath: "pkgOwner",
	}

	f, err := oswrap.Create(filepath.Join(srcDir, "conflicting_file"))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := f.Stat()
	f.Close()

	// Test 1: Conflict without force -> Success by default.
	fnBlock := makeInstallFunction(srcDir, dstDir, newInstallTxn(defaultInstallOps(), false, false, cm))
	errBlock := fnBlock(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errBlock != nil {
		t.Errorf("expected no conflict error by default, got %v", errBlock)
	}

	// Test 2: Conflict with force -> Success.
	fnForce := makeInstallFunction(srcDir, dstDir, newInstallTxn(defaultInstallOps(), false, true, cm))
	errForce := fnForce(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errForce != nil {
		t.Errorf("expected no error with force, got %v", errForce)
	}

	// Test 3: Conflict without force in strict mode -> Error.
	settings.StrictConflicts = true
	defer func() { settings.StrictConflicts = false }()
	fnStrict := makeInstallFunction(srcDir, dstDir, newInstallTxn(defaultInstallOps(), false, false, cm))
	errStrict := fnStrict(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errStrict == nil {
		t.Errorf("expected conflict error in strict mode, got nil")
	}

	// Test 4: Conflict with force in strict mode -> Success.
	fnStrictForce := makeInstallFunction(srcDir, dstDir, newInstallTxn(defaultInstallOps(), false, true, cm))
	errStrictForce := fnStrictForce(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errStrictForce != nil {
		t.Errorf("expected no error with force in strict mode, got %v", errStrictForce)
	}
}

// TestInstallPkg_RollbackOnFailure verifies that on installer failure, newly placed files
// are removed, replaced files are restored from backup, and backup files are cleaned up.
func TestInstallPkg_RollbackOnFailure(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Create a pre-existing file that will be replaced.
	existingFile := filepath.Join(dstDir, "existing.txt")
	originalContent := []byte("original content version 1")
	if err := os.WriteFile(existingFile, originalContent, 0644); err != nil {
		t.Fatalf("Failed to create existing file: %v", err)
	}

	// Build a .goo package containing both an existing file replacement and a newly placed file.
	pkgFile := filepath.Join(srcDir, "rollback_test.goo")
	f, err := os.Create(pkgFile)
	if err != nil {
		t.Fatalf("Failed to create package file: %v", err)
	}

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	filesToPack := []struct {
		name    string
		content []byte
	}{
		{"existing.txt", []byte("overwritten content version 2")},
		{"new_file.txt", []byte("brand new file payload")},
	}

	for _, entry := range filesToPack {
		hdr := &tar.Header{
			Name: entry.name,
			Mode: 0644,
			Size: int64(len(entry.content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("Failed to write tar header for %s: %v", entry.name, err)
		}
		if _, err := tw.Write(entry.content); err != nil {
			t.Fatalf("Failed to write tar content for %s: %v", entry.name, err)
		}
	}
	tw.Close()
	gw.Close()
	f.Close()

	// Use a backup operation that creates the backup at a known path.
	backupFile := existingFile + ".old_backup"
	ops := defaultInstallOps()
	ops.backup = func(filename string) (string, error) {
		if filename == existingFile {
			if err := oswrap.Rename(filename, backupFile); err != nil {
				return "", err
			}
			return backupFile, nil
		}
		return renameToBackup(filename)
	}

	ps := &goolib.PkgSpec{
		Name:    "rollback_pkg",
		Version: "1.0.0@1",
		Arch:    "noarch",
		Files: map[string]string{
			"existing.txt": filepath.Join(dstDir, "existing.txt"),
			"new_file.txt": filepath.Join(dstDir, "new_file.txt"),
		},
		Install: goolib.ExecFile{
			Path: "nonexistent_installer_binary_that_will_fail.exe",
		},
	}

	// Execute installPkg; must return an error.
	_, err = installPkg(ops, pkgFile, ps, false, false, db)
	if err == nil {
		t.Fatalf("installPkg succeeded unexpectedly; expected installer failure error")
	}

	// Verify all newly placed files in insFiles are deleted.
	newFilePath := filepath.Join(dstDir, "new_file.txt")
	if _, err := os.Stat(newFilePath); !os.IsNotExist(err) {
		t.Errorf("Newly placed file %s was not deleted during rollback", newFilePath)
	}

	// Verify replaced file is restored to its original content.
	restoredData, err := os.ReadFile(existingFile)
	if err != nil {
		t.Fatalf("Expected restored file %s to exist: %v", existingFile, err)
	}
	if string(restoredData) != string(originalContent) {
		t.Errorf("Restored file content = %q, want original content %q", string(restoredData), string(originalContent))
	}

	// Verify temporary backup file is cleaned up.
	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Errorf("Temporary backup file %s still exists after rollback", backupFile)
	}
}

// TestInstallPkg_SuccessCleansBackup verifies that upon successful installation,
// new files are written and temporary backup files are deleted.
func TestInstallPkg_SuccessCleansBackup(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	existingFile := filepath.Join(dstDir, "existing.txt")
	if err := os.WriteFile(existingFile, []byte("original content"), 0644); err != nil {
		t.Fatalf("Failed to create existing file: %v", err)
	}

	pkgFile := filepath.Join(srcDir, "success_test.goo")
	f, err := os.Create(pkgFile)
	if err != nil {
		t.Fatalf("Failed to create package file: %v", err)
	}

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	newContent := []byte("overwritten new content")
	hdr := &tar.Header{
		Name: "existing.txt",
		Mode: 0644,
		Size: int64(len(newContent)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("Failed to write tar header: %v", err)
	}
	if _, err := tw.Write(newContent); err != nil {
		t.Fatalf("Failed to write tar content: %v", err)
	}
	tw.Close()
	gw.Close()
	f.Close()

	backupFile := existingFile + ".old_backup"
	ops := defaultInstallOps()
	ops.backup = func(filename string) (string, error) {
		if filename == existingFile {
			if err := oswrap.Rename(filename, backupFile); err != nil {
				return "", err
			}
			return backupFile, nil
		}
		return renameToBackup(filename)
	}

	ps := &goolib.PkgSpec{
		Name:    "success_pkg",
		Version: "1.0.0@1",
		Arch:    "noarch",
		Files:   map[string]string{"existing.txt": filepath.Join(dstDir, "existing.txt")},
	}

	insFiles, err := installPkg(ops, pkgFile, ps, false, false, db)
	if err != nil {
		t.Fatalf("installPkg error: %v", err)
	}

	data, err := os.ReadFile(existingFile)
	if err != nil {
		t.Fatalf("Expected %s to exist: %v", existingFile, err)
	}
	if string(data) != "overwritten new content" {
		t.Errorf("File content = %q, want %q", string(data), "overwritten new content")
	}

	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Errorf("Backup file %s was not deleted on success", backupFile)
	}

	if _, ok := insFiles[existingFile]; !ok {
		t.Errorf("insFiles did not contain %s", existingFile)
	}
}

// TestFromDisk_RollbackAndDBUntouched verifies that when a package installation fails
// via FromDisk, placed files are rolled back and the package is not written to googet.db.
func TestFromDisk_RollbackAndDBUntouched(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	cacheDir := t.TempDir()

	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	pkgFile := filepath.Join(srcDir, "test_pkg_noarch.goo")
	f, err := os.Create(pkgFile)
	if err != nil {
		t.Fatalf("Failed to create package file: %v", err)
	}

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	specContent := []byte(`{
		"name": "test_pkg",
		"version": "1.0.0@1",
		"arch": "noarch",
		"files": {
			"payload.txt": "` + filepath.Join(dstDir, "payload.txt") + `"
		},
		"install": {
			"path": "nonexistent_installer_binary_that_will_fail.exe"
		}
	}`)
	hdrSpec := &tar.Header{
		Name: "test_pkg.pkgspec",
		Mode: 0644,
		Size: int64(len(specContent)),
	}
	if err := tw.WriteHeader(hdrSpec); err != nil {
		t.Fatalf("Failed to write spec header: %v", err)
	}
	if _, err := tw.Write(specContent); err != nil {
		t.Fatalf("Failed to write spec content: %v", err)
	}

	payloadContent := []byte("payload content")
	hdrPayload := &tar.Header{
		Name: "payload.txt",
		Mode: 0644,
		Size: int64(len(payloadContent)),
	}
	if err := tw.WriteHeader(hdrPayload); err != nil {
		t.Fatalf("Failed to write payload header: %v", err)
	}
	if _, err := tw.Write(payloadContent); err != nil {
		t.Fatalf("Failed to write payload content: %v", err)
	}
	tw.Close()
	gw.Close()
	f.Close()

	err = FromDisk(pkgFile, cacheDir, false, false, false, db)
	if err == nil {
		t.Fatalf("FromDisk expected error, got nil")
	}

	// Verify that a failed install does not record the package in the database.
	pState, err := db.FetchPkg(goolib.PackageInfo{Name: "test_pkg", Arch: "noarch", Ver: "1.0.0@1"})
	if err != nil {
		t.Fatalf("db.FetchPkg error: %v", err)
	}
	if pState.PackageSpec != nil {
		t.Errorf("Package %s was added to DB despite failed install: %+v", "test_pkg", pState)
	}

	// Verify that rollback deleted the file the failed install placed.
	placedFile := filepath.Join(dstDir, "payload.txt")
	if _, err := os.Stat(placedFile); !os.IsNotExist(err) {
		t.Errorf("Placed file %s still exists after failed install", placedFile)
	}
}

// createStressGooArchive packages a .pkgspec and arbitrary files into a .goo archive.
func createStressGooArchive(t *testing.T, archivePath string, specContent []byte, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Failed to create goo package file %s: %v", archivePath, err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	if specContent != nil {
		hdr := &tar.Header{
			Name: "stress_pkg.pkgspec",
			Mode: 0644,
			Size: int64(len(specContent)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("Failed to write pkgspec header: %v", err)
		}
		if _, err := tw.Write(specContent); err != nil {
			t.Fatalf("Failed to write pkgspec content: %v", err)
		}
	}

	dirsWritten := make(map[string]bool)
	for relPath := range files {
		clean := filepath.ToSlash(filepath.Clean(relPath))
		parts := strings.Split(filepath.Dir(clean), "/")
		cur := ""
		for _, part := range parts {
			if part == "." || part == "" {
				continue
			}
			if cur == "" {
				cur = part
			} else {
				cur = cur + "/" + part
			}
			if !dirsWritten[cur] {
				dirsWritten[cur] = true
				hdr := &tar.Header{
					Name:     cur + "/",
					Typeflag: tar.TypeDir,
					Mode:     0755,
				}
				if err := tw.WriteHeader(hdr); err != nil {
					t.Fatalf("Failed to write dir header for %s: %v", cur, err)
				}
			}
		}
	}

	for relPath, content := range files {
		hdr := &tar.Header{
			Name: relPath,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("Failed to write tar header for %s: %v", relPath, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("Failed to write tar content for %s: %v", relPath, err)
		}
	}
}

// TestStress_RollbackNestedDirectoryTree stress-tests rollback across a complex nested file tree.
func TestStress_RollbackNestedDirectoryTree(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	cacheDir := t.TempDir()

	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Pre-create existing files in various nested directories.
	existingFiles := map[string][]byte{
		filepath.Join(dstDir, "bin", "app.exe"):                    []byte("v1.0 binary payload"),
		filepath.Join(dstDir, "etc", "app", "conf.d", "main.conf"): []byte("v1.0 config contents"),
		filepath.Join(dstDir, "lib", "modules", "mod.so"):          []byte("v1.0 module binary"),
		filepath.Join(dstDir, "var", "log", "keep.log"):            []byte("unrelated application log"),
	}
	for path, content := range existingFiles {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("Failed to create dir for %s: %v", path, err)
		}
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatalf("Failed to write existing file %s: %v", path, err)
		}
	}

	// Define files in package: 3 replacements and 2 brand new files.
	pkgFiles := map[string][]byte{
		"bin/app.exe":              []byte("v2.0 overwritten binary payload"),
		"etc/app/conf.d/main.conf": []byte("v2.0 overwritten config contents"),
		"lib/modules/mod.so":       []byte("v2.0 overwritten module binary"),
		"opt/extra/addon.txt":      []byte("brand new addon text file"),
		"bin/helper.exe":           []byte("brand new helper tool"),
	}

	specJSON := fmt.Sprintf(`{
		"name": "stress_nested_pkg",
		"version": "2.0.0@1",
		"arch": "noarch",
		"files": {
			"./": %q
		},
		"install": {
			"path": "nonexistent_failing_installer.exe"
		}
	}`, dstDir)

	pkgPath := filepath.Join(srcDir, "stress_nested_pkg.goo")
	createStressGooArchive(t, pkgPath, []byte(specJSON), pkgFiles)

	// Use a backup operation that creates backups at known paths.
	var createdBackups []string
	ops := defaultInstallOps()
	ops.backup = func(filename string) (string, error) {
		if _, ok := existingFiles[filename]; ok {
			backup := filename + ".old_backup"
			if err := oswrap.Rename(filename, backup); err != nil {
				return "", err
			}
			createdBackups = append(createdBackups, backup)
			return backup, nil
		}
		return renameToBackup(filename)
	}

	// Execute fromDisk; must fail.
	err = fromDisk(ops, pkgPath, cacheDir, false, false, false, db)
	if err == nil {
		t.Fatalf("FromDisk expected installer failure error, got nil")
	}
	t.Logf("FromDisk returned error: %v", err)

	// 1. Verify newly placed files are deleted.
	newFiles := []string{
		filepath.Join(dstDir, "opt", "extra", "addon.txt"),
		filepath.Join(dstDir, "bin", "helper.exe"),
	}
	for _, nf := range newFiles {
		if _, err := os.Stat(nf); !os.IsNotExist(err) {
			t.Errorf("Newly placed file %s was not deleted during rollback", nf)
		}
	}

	// 2. Verify replaced files are restored to exact original content.
	for path, expectedContent := range existingFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("Replaced file %s does not exist after rollback: %v", path, err)
			continue
		}
		if string(data) != string(expectedContent) {
			t.Errorf("File %s content mismatch after rollback: got %q, want %q", path, string(data), string(expectedContent))
		}
	}

	// 3. Verify all temporary backups were cleaned up.
	for _, b := range createdBackups {
		if _, err := os.Stat(b); !os.IsNotExist(err) {
			t.Errorf("Temporary backup file %s still exists after rollback", b)
		}
	}

	// 4. Verify package is not recorded in googet.db.
	pi := goolib.PackageInfo{Name: "stress_nested_pkg", Arch: "noarch", Ver: "2.0.0@1"}
	st, err := db.FetchPkg(pi)
	if err != nil {
		t.Fatalf("db.FetchPkg error: %v", err)
	}
	if st.PackageSpec != nil {
		t.Errorf("Package %s was added to googet.db despite install failure", pi.Name)
	}
}

// TestStress_DatabaseIntegrityOnAllFailureModes verifies DB safety across all failure phases.
func TestStress_DatabaseIntegrityOnAllFailureModes(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Pre-populate DB with two existing baseline packages.
	baselineState := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "pkg_alpha",
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "pkg_beta",
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
	}
	if err := db.WriteStateToDB(baselineState); err != nil {
		t.Fatalf("db.WriteStateToDB error: %v", err)
	}

	assertDBUntouched := func(scenario string) {
		t.Helper()
		pkgs, err := db.FetchPkgs("")
		if err != nil {
			t.Fatalf("[%s] db.FetchPkgs error: %v", scenario, err)
		}
		if len(pkgs) != 2 {
			t.Errorf("[%s] Expected 2 packages in DB, found %d: %+v", scenario, len(pkgs), pkgs)
		}
		for _, name := range []string{"pkg_alpha", "pkg_beta"} {
			pi := goolib.PackageInfo{Name: name, Arch: "noarch", Ver: "1.0.0@1"}
			p, err := db.FetchPkg(pi)
			if err != nil || p.PackageSpec == nil {
				t.Errorf("[%s] Expected baseline package %s to remain in DB", scenario, name)
			}
		}
	}

	cacheDir := t.TempDir()

	// Failure Mode 1: Corrupted package file.
	{
		corruptPkg := filepath.Join(t.TempDir(), "corrupt.goo")
		if err := os.WriteFile(corruptPkg, []byte("not a valid gzip tar file"), 0644); err != nil {
			t.Fatalf("Failed to write corrupt package: %v", err)
		}
		if err := FromDisk(corruptPkg, cacheDir, false, false, false, db); err == nil {
			t.Errorf("Expected FromDisk error on corrupt archive, got nil")
		}
		assertDBUntouched("CorruptArchive")
	}

	// Failure Mode 2: Replaces installed package conflict.
	{
		conflictPkg := filepath.Join(t.TempDir(), "conflict.goo")
		specJSON := `{
			"name": "pkg_gamma",
			"version": "1.0.0@1",
			"arch": "noarch",
			"replaces": ["pkg_alpha"]
		}`
		createStressGooArchive(t, conflictPkg, []byte(specJSON), nil)
		if err := FromDisk(conflictPkg, cacheDir, false, false, false, db); err == nil {
			t.Errorf("Expected FromDisk error on conflicting replaces, got nil")
		}
		assertDBUntouched("ConflictingReplaces")
	}

	// Failure Mode 3: Unsatisfied dependency.
	{
		missingDepPkg := filepath.Join(t.TempDir(), "missing_dep.goo")
		specJSON := `{
			"name": "pkg_delta",
			"version": "1.0.0@1",
			"arch": "noarch",
			"PkgDependencies": {
				"nonexistent_dep.noarch": "1.0.0@1"
			}
		}`
		createStressGooArchive(t, missingDepPkg, []byte(specJSON), nil)
		if err := FromDisk(missingDepPkg, cacheDir, false, false, false, db); err == nil {
			t.Errorf("Expected FromDisk error on missing dependency, got nil")
		}
		assertDBUntouched("MissingDependency")
	}

	// Failure Mode 4: Failing installer binary.
	{
		failInstallPkg := filepath.Join(t.TempDir(), "fail_install.goo")
		specJSON := `{
			"name": "pkg_epsilon",
			"version": "1.0.0@1",
			"arch": "noarch",
			"install": {
				"path": "does_not_exist_installer.exe"
			}
		}`
		createStressGooArchive(t, failInstallPkg, []byte(specJSON), nil)
		if err := FromDisk(failInstallPkg, cacheDir, false, false, false, db); err == nil {
			t.Errorf("Expected FromDisk error on installer failure, got nil")
		}
		assertDBUntouched("FailingInstaller")
	}
}

// TestStress_MultipleConsecutiveFailuresThenSuccess verifies rollback idempotence and eventual success.
func TestStress_MultipleConsecutiveFailuresThenSuccess(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	cacheDir := t.TempDir()

	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	targetFile := filepath.Join(dstDir, "stateful.txt")
	newFile := filepath.Join(dstDir, "extra.txt")
	originalContent := []byte("original pristine state")
	if err := os.WriteFile(targetFile, originalContent, 0644); err != nil {
		t.Fatalf("Failed to write initial target file: %v", err)
	}

	failingSpec := fmt.Sprintf(`{
		"name": "idempotence_pkg",
		"version": "1.0.0@1",
		"arch": "noarch",
		"files": {
			"stateful.txt": %q,
			"extra.txt": %q
		},
		"install": {
			"path": "nonexistent_failing_tool.exe"
		}
	}`, targetFile, newFile)

	failingPkg := filepath.Join(srcDir, "fail.goo")
	createStressGooArchive(t, failingPkg, []byte(failingSpec), map[string][]byte{
		"stateful.txt": []byte("corrupted attempt"),
		"extra.txt":    []byte("unwanted extra"),
	})

	backupFile := targetFile + ".old_backup"
	ops := defaultInstallOps()
	ops.backup = func(filename string) (string, error) {
		if filename == targetFile {
			if err := oswrap.Rename(filename, backupFile); err != nil {
				return "", err
			}
			return backupFile, nil
		}
		return renameToBackup(filename)
	}

	// Attempt 1: First failure.
	if err := fromDisk(ops, failingPkg, cacheDir, false, false, false, db); err == nil {
		t.Fatalf("Attempt 1 expected error, got nil")
	}
	data, err := os.ReadFile(targetFile)
	if err != nil || string(data) != string(originalContent) {
		t.Fatalf("Attempt 1 rollback failed: content = %q, want %q", string(data), string(originalContent))
	}
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatalf("Attempt 1 extra file still exists")
	}
	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Fatalf("Attempt 1 backup file still exists")
	}

	// Attempt 2: Second consecutive failure (must not corrupt restored file).
	if err := fromDisk(ops, failingPkg, cacheDir, false, false, false, db); err == nil {
		t.Fatalf("Attempt 2 expected error, got nil")
	}
	data, err = os.ReadFile(targetFile)
	if err != nil || string(data) != string(originalContent) {
		t.Fatalf("Attempt 2 rollback failed: content = %q, want %q", string(data), string(originalContent))
	}
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatalf("Attempt 2 extra file still exists")
	}
	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Fatalf("Attempt 2 backup file still exists")
	}

	// Attempt 3: Successful package installation.
	successSpec := fmt.Sprintf(`{
		"name": "idempotence_pkg",
		"version": "1.0.0@1",
		"arch": "noarch",
		"files": {
			"stateful.txt": %q,
			"extra.txt": %q
		}
	}`, targetFile, newFile)

	successPkg := filepath.Join(srcDir, "success.goo")
	createStressGooArchive(t, successPkg, []byte(successSpec), map[string][]byte{
		"stateful.txt": []byte("final successful v1.0 state"),
		"extra.txt":    []byte("final successful extra file"),
	})

	if err := fromDisk(ops, successPkg, cacheDir, false, false, false, db); err != nil {
		t.Fatalf("Attempt 3 expected success, got error: %v", err)
	}

	data, err = os.ReadFile(targetFile)
	if err != nil || string(data) != "final successful v1.0 state" {
		t.Errorf("Target file content = %q, want %q", string(data), "final successful v1.0 state")
	}
	extraData, err := os.ReadFile(newFile)
	if err != nil || string(extraData) != "final successful extra file" {
		t.Errorf("Extra file content = %q, want %q", string(extraData), "final successful extra file")
	}
	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Errorf("Backup file still exists after successful install")
	}

	pState, err := db.FetchPkg(goolib.PackageInfo{Name: "idempotence_pkg", Arch: "noarch", Ver: "1.0.0@1"})
	if err != nil || pState.PackageSpec == nil {
		t.Errorf("Package idempotence_pkg was not recorded in DB on success")
	}
}

// TestStress_FailureMidwayThroughWalk verifies rollback when failure occurs midway through file copying.
func TestStress_FailureMidwayThroughWalk(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	cacheDir := t.TempDir()

	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	file1 := filepath.Join(dstDir, "file1.txt")
	file2 := filepath.Join(dstDir, "file2.txt")
	originalContent1 := []byte("original file 1")
	originalContent2 := []byte("original file 2")
	if err := os.WriteFile(file1, originalContent1, 0644); err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}
	if err := os.WriteFile(file2, originalContent2, 0644); err != nil {
		t.Fatalf("Failed to write file2: %v", err)
	}

	specJSON := fmt.Sprintf(`{
		"name": "midway_fail_pkg",
		"version": "1.0.0@1",
		"arch": "noarch",
		"files": {
			"file1.txt": %q,
			"file2.txt": %q
		}
	}`, file1, file2)

	pkgPath := filepath.Join(srcDir, "midway_fail.goo")
	createStressGooArchive(t, pkgPath, []byte(specJSON), map[string][]byte{
		"file1.txt": []byte("new file 1"),
		"file2.txt": []byte("new file 2"),
	})

	backupFile1 := file1 + ".old_backup"
	simulatedErr := errors.New("simulated access denied on file2")

	ops := defaultInstallOps()
	ops.backup = func(filename string) (string, error) {
		if filename == file1 {
			if err := oswrap.Rename(filename, backupFile1); err != nil {
				return "", err
			}
			return backupFile1, nil
		}
		if filename == file2 {
			return "", simulatedErr
		}
		return renameToBackup(filename)
	}
	// Make the remove-or-rename fallback fail for file2 as well.
	ops.removeOrRename = func(filename string) (string, error) {
		if filename == file2 {
			return "", simulatedErr
		}
		return client.RemoveOrRename(filename)
	}

	err = fromDisk(ops, pkgPath, cacheDir, false, false, false, db)
	if err == nil {
		t.Fatalf("Expected FromDisk error on simulated failure, got nil")
	}
	if !strings.Contains(err.Error(), "simulated access denied on file2") {
		t.Errorf("Error does not contain simulated error: %v", err)
	}

	// Verify file1 was restored from backup.
	data1, err := os.ReadFile(file1)
	if err != nil || string(data1) != string(originalContent1) {
		t.Errorf("File 1 was not restored: got %q, want %q", string(data1), string(originalContent1))
	}
	if _, err := os.Stat(backupFile1); !os.IsNotExist(err) {
		t.Errorf("Backup file 1 still exists after rollback")
	}

	// Verify file2 remains untouched.
	data2, err := os.ReadFile(file2)
	if err != nil || string(data2) != string(originalContent2) {
		t.Errorf("File 2 was modified unexpectedly: got %q, want %q", string(data2), string(originalContent2))
	}
	// The fallback copy of file2 must be discarded once removeOrRename fails.
	assertNoBackups(t, dstDir)

	// Verify DB was untouched.
	pState, err := db.FetchPkg(goolib.PackageInfo{Name: "midway_fail_pkg", Arch: "noarch", Ver: "1.0.0@1"})
	if err != nil {
		t.Fatalf("db.FetchPkg error: %v", err)
	}
	if pState.PackageSpec != nil {
		t.Errorf("Package midway_fail_pkg was recorded in DB despite failure")
	}
}

// failingOps returns the default operations with a system installer that
// always fails.
func failingOps() installOps {
	ops := defaultInstallOps()
	ops.systemInstall = func(string, *goolib.PkgSpec) error {
		return errors.New("simulated installer failure")
	}
	return ops
}

// succeedingOps returns the default operations with a system installer that
// always succeeds.
func succeedingOps() installOps {
	ops := defaultInstallOps()
	ops.systemInstall = func(string, *goolib.PkgSpec) error { return nil }
	return ops
}

// newTestDB returns a fresh googet database for the test. It does not touch
// the settings package so that callers may run in parallel.
func newTestDB(t *testing.T) *googetdb.GooDB {
	t.Helper()
	db, err := googetdb.NewDB(filepath.Join(t.TempDir(), "googet.db"))
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// writeFiles creates each file with its content, creating parent directories.
func writeFiles(t *testing.T, files map[string][]byte) {
	t.Helper()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}
}

// assertContents fails the test unless each file exists with exactly the given content.
func assertContents(t *testing.T, files map[string][]byte) {
	t.Helper()
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile(%q): %v", path, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Content of %q = %q, want %q", path, got, want)
		}
	}
}

// assertAbsent fails the test if any of the paths exist.
func assertAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("Path %q exists (Lstat error: %v), want it absent", p, err)
		}
	}
}

// assertNoBackups fails the test if any backup file remains under root.
func assertNoBackups(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(fi.Name(), backupInfix) {
			t.Errorf("Leftover backup file %q", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(%q): %v", root, err)
	}
}

// extractionDir returns the directory installPkg extracts pkgPath into.
func extractionDir(pkgPath string) string {
	return strings.TrimSuffix(pkgPath, filepath.Ext(pkgPath))
}

// TestInstallPkg_UpgradeFailureRestoresOldFiles verifies that when an upgrade
// fails in the installer, the previous version's files are restored byte for
// byte, files new to the upgrade are removed, and the extraction directory is
// preserved for diagnosis.
func TestInstallPkg_UpgradeFailureRestoresOldFiles(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dstDir := t.TempDir()
	a, b, newOnly := filepath.Join(dstDir, "a.txt"), filepath.Join(dstDir, "b.txt"), filepath.Join(dstDir, "new.txt")
	old := map[string][]byte{a: []byte("v1 contents of a"), b: []byte("v1 contents of b")}
	writeFiles(t, old)
	if err := db.WriteStateToDB([]client.PackageState{{
		PackageSpec:    &goolib.PkgSpec{Name: "upgrade_pkg", Version: "1.0.0@1", Arch: "noarch"},
		InstalledFiles: map[string]string{a: "chksum-a", b: "chksum-b"},
	}}); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	pkgPath := filepath.Join(t.TempDir(), "upgrade_pkg.goo")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{
		"a.txt":   []byte("v2 contents of a, longer than v1"),
		"b.txt":   []byte("v2 b"),
		"new.txt": []byte("only in v2"),
	})

	ps := &goolib.PkgSpec{Name: "upgrade_pkg", Version: "2.0.0@1", Arch: "noarch", Files: map[string]string{"./": dstDir}}
	if _, err := installPkg(failingOps(), pkgPath, ps, false, false, db); err == nil {
		t.Fatal("installPkg succeeded, want installer failure")
	}

	assertContents(t, old)
	assertAbsent(t, newOnly)
	assertNoBackups(t, dstDir)
	if fi, err := os.Stat(extractionDir(pkgPath)); err != nil || !fi.IsDir() {
		t.Errorf("Extraction dir %q not preserved after failure: %v", extractionDir(pkgPath), err)
	}
	assertContents(t, map[string][]byte{filepath.Join(extractionDir(pkgPath), "new.txt"): []byte("only in v2")})
}

// TestReinstall_FailureKeepsInstalledFiles verifies that a failed Reinstall
// leaves the currently installed files intact. Reinstall uses the production
// operations, so the failure comes from an installer that does not exist.
func TestReinstall_FailureKeepsInstalledFiles(t *testing.T) {
	db := newTestDB(t)
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "bin", "tool.exe")
	installed := map[string][]byte{target: []byte("currently installed tool")}
	writeFiles(t, installed)

	pkgPath := filepath.Join(t.TempDir(), "reinstall_pkg.goo")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{"tool.exe": []byte("tool from package cache")})

	spec := &goolib.PkgSpec{
		Name:    "reinstall_pkg",
		Version: "1.0.0@1",
		Arch:    "noarch",
		Files:   map[string]string{"tool.exe": target},
		Install: goolib.ExecFile{Path: "nonexistent_installer_binary_that_will_fail.exe"},
	}
	st := client.PackageState{LocalPath: pkgPath, PackageSpec: spec, InstalledFiles: map[string]string{target: "chksum"}}
	if err := Reinstall(t.Context(), st, false, false, nil, db); err == nil {
		t.Fatal("Reinstall succeeded, want installer failure")
	}

	assertContents(t, installed)
	assertNoBackups(t, dstDir)
}

// TestInstallPkg_ConflictOverwriteRestoredOnFailure verifies that a file owned
// by another package, overwritten because StrictConflicts is off, is restored
// when the install fails. It writes settings.StrictConflicts and therefore
// does not run in parallel.
func TestInstallPkg_ConflictOverwriteRestoredOnFailure(t *testing.T) {
	db := newTestDB(t)
	settings.StrictConflicts = false
	dstDir := t.TempDir()
	shared := filepath.Join(dstDir, "shared.dll")
	owned := map[string][]byte{shared: []byte("owned by other_pkg")}
	writeFiles(t, owned)
	if err := db.WriteStateToDB([]client.PackageState{{
		PackageSpec:    &goolib.PkgSpec{Name: "other_pkg", Version: "1.0.0@1", Arch: "noarch"},
		InstalledFiles: map[string]string{shared: "chksum"},
	}}); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	pkgPath := filepath.Join(t.TempDir(), "new_pkg.goo")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{"shared.dll": []byte("from new_pkg")})

	ps := &goolib.PkgSpec{Name: "new_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"shared.dll": shared}}
	if _, err := installPkg(failingOps(), pkgPath, ps, false, false, db); err == nil {
		t.Fatal("installPkg succeeded, want installer failure")
	}

	assertContents(t, owned)
	assertNoBackups(t, dstDir)
}

// TestInstallPkg_PartialCopyRolledBack verifies that a copy failing midway
// removes the partially written new file and restores a replaced file.
func TestInstallPkg_PartialCopyRolledBack(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dstDir := t.TempDir()
	existing, fresh := filepath.Join(dstDir, "existing.txt"), filepath.Join(dstDir, "fresh.txt")
	orig := map[string][]byte{existing: []byte("original existing content")}
	writeFiles(t, orig)

	ops := succeedingOps()
	ops.copyContents = func(w io.Writer, r io.Reader) (int64, error) {
		n, err := io.CopyN(w, r, 4)
		if err != nil {
			return n, err
		}
		return n, errors.New("simulated copy failure")
	}

	// The subtests share dstDir and therefore run sequentially.
	for _, target := range []string{existing, fresh} {
		t.Run(filepath.Base(target), func(t *testing.T) {
			pkgPath := filepath.Join(t.TempDir(), "partial_pkg.goo")
			createStressGooArchive(t, pkgPath, nil, map[string][]byte{"payload.txt": []byte("0123456789 full payload")})

			ps := &goolib.PkgSpec{Name: "partial_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"payload.txt": target}}
			_, err := installPkg(ops, pkgPath, ps, false, false, db)
			if err == nil || !strings.Contains(err.Error(), "simulated copy failure") {
				t.Fatalf("installPkg error = %v, want simulated copy failure", err)
			}
			assertContents(t, orig)
			assertAbsent(t, fresh)
			assertNoBackups(t, dstDir)
		})
	}
}

// TestInstallPkg_NewDirectoriesRemovedOnRollback verifies that directories
// created by a failed install are removed while pre-existing directories and
// their unrelated contents are kept.
func TestInstallPkg_NewDirectoriesRemovedOnRollback(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	keep := filepath.Join(appDir, "existing_dir", "keep.txt")
	kept := map[string][]byte{keep: []byte("unrelated file")}
	writeFiles(t, kept)

	pkgPath := filepath.Join(t.TempDir(), "dirs_pkg.goo")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{
		"existing_dir/new.txt":  []byte("new in existing dir"),
		"newdir/deep/f.txt":     []byte("deep new file"),
		"newdir/other/leaf.txt": []byte("another new file"),
	})

	target := filepath.Join(appDir, "nested", "install_root")
	ps := &goolib.PkgSpec{Name: "dirs_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{
		"./": target,
		// Also install into the pre-existing directory tree.
		"existing_dir": filepath.Join(appDir, "existing_dir"),
	}}
	if _, err := installPkg(failingOps(), pkgPath, ps, false, false, db); err == nil {
		t.Fatal("installPkg succeeded, want installer failure")
	}

	assertContents(t, kept)
	assertAbsent(t, filepath.Join(appDir, "nested"), filepath.Join(appDir, "existing_dir", "new.txt"))
	if fi, err := os.Stat(filepath.Join(appDir, "existing_dir")); err != nil || !fi.IsDir() {
		t.Errorf("Pre-existing directory was removed: %v", err)
	}
}

// TestInstallPkg_SuccessRemovesBackupsAndExtractionDir verifies that a
// successful install writes the new contents, leaves no backup files, and
// removes the extraction directory.
func TestInstallPkg_SuccessRemovesBackupsAndExtractionDir(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dstDir := t.TempDir()
	a, b := filepath.Join(dstDir, "a.txt"), filepath.Join(dstDir, "b.txt")
	writeFiles(t, map[string][]byte{a: []byte("old a")})

	pkgPath := filepath.Join(t.TempDir(), "ok_pkg.goo")
	newContents := map[string][]byte{"a.txt": []byte("new a"), "b.txt": []byte("new b")}
	createStressGooArchive(t, pkgPath, nil, newContents)

	ps := &goolib.PkgSpec{Name: "ok_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"./": dstDir}}
	insFiles, err := installPkg(succeedingOps(), pkgPath, ps, false, false, db)
	if err != nil {
		t.Fatalf("installPkg: %v", err)
	}

	assertContents(t, map[string][]byte{a: newContents["a.txt"], b: newContents["b.txt"]})
	assertNoBackups(t, dstDir)
	assertAbsent(t, extractionDir(pkgPath))
	for _, f := range []string{a, b} {
		if insFiles[f] == "" {
			t.Errorf("insFiles[%q] is empty, want a checksum", f)
		}
	}
}

// TestInstallPkg_SamePathTwiceRestoresOriginal verifies that when two package
// entries write the same destination, rollback restores the pre-install file
// rather than the first entry's content.
func TestInstallPkg_SamePathTwiceRestoresOriginal(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "x.txt")
	orig := map[string][]byte{target: []byte("pre-install x")}
	writeFiles(t, orig)

	pkgPath := filepath.Join(t.TempDir(), "dup_pkg.goo")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{"one.txt": []byte("first"), "two.txt": []byte("second")})

	ps := &goolib.PkgSpec{Name: "dup_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"one.txt": target, "two.txt": target}}
	if _, err := installPkg(failingOps(), pkgPath, ps, false, false, db); err == nil {
		t.Fatal("installPkg succeeded, want installer failure")
	}

	assertContents(t, orig)
	assertNoBackups(t, dstDir)
}

// TestInstallPkg_BackupFallback verifies the behavior when renaming a file to
// a backup fails and the remove-or-rename fallback preserves the original
// under a new name: that name is restored, the redundant copy is discarded,
// and the install still proceeds.
func TestInstallPkg_BackupFallback(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "locked.exe")
	orig := map[string][]byte{target: []byte("locked original")}
	writeFiles(t, orig)
	fallbackBackup := filepath.Join(dstDir, "fallback.old")

	ops := failingOps()
	ops.backup = func(string) (string, error) { return "", errors.New("simulated rename failure") }
	ops.removeOrRename = func(filename string) (string, error) {
		if err := oswrap.Rename(filename, fallbackBackup); err != nil {
			return "", err
		}
		return fallbackBackup, nil
	}

	pkgPath := filepath.Join(t.TempDir(), "fallback_pkg.goo")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{"locked.exe": []byte("replacement")})

	ps := &goolib.PkgSpec{Name: "fallback_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"locked.exe": target}}
	if _, err := installPkg(ops, pkgPath, ps, false, false, db); err == nil {
		t.Fatal("installPkg succeeded, want installer failure")
	}

	assertContents(t, orig)
	assertAbsent(t, fallbackBackup)
	assertNoBackups(t, dstDir)
}

// TestInstallPkg_BackupFallbackDeletedFileRestored verifies that when renaming
// a file to a backup fails and the fallback deletes the file, the copy made
// beforehand restores it byte for byte, with its mode, on rollback, and is
// removed after a successful install.
func TestInstallPkg_BackupFallbackDeletedFileRestored(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		succeed bool
	}{
		{name: "rollback", succeed: false},
		{name: "commit", succeed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t)
			dstDir := t.TempDir()
			target := filepath.Join(dstDir, "app.dll")
			// Include every byte value to catch any transformation of the data.
			content := make([]byte, 4096)
			for i := range content {
				content[i] = byte(i)
			}
			writeFiles(t, map[string][]byte{target: content})
			if err := os.Chmod(target, 0640); err != nil {
				t.Fatalf("Chmod: %v", err)
			}

			ops := failingOps()
			if tc.succeed {
				ops = succeedingOps()
			}
			ops.backup = func(string) (string, error) { return "", errors.New("simulated rename failure") }
			var deleted bool
			ops.removeOrRename = func(filename string) (string, error) {
				if err := os.Remove(filename); err != nil {
					return "", err
				}
				deleted = true
				return "", nil
			}

			pkgPath := filepath.Join(t.TempDir(), "deleted_pkg.goo")
			replacement := []byte("replacement payload")
			createStressGooArchive(t, pkgPath, nil, map[string][]byte{"app.dll": replacement})

			ps := &goolib.PkgSpec{Name: "deleted_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"app.dll": target}}
			_, err := installPkg(ops, pkgPath, ps, false, false, db)
			if !deleted {
				t.Fatal("removeOrRename was not called; the fallback path was not exercised")
			}
			if tc.succeed {
				if err != nil {
					t.Fatalf("installPkg: %v", err)
				}
				assertContents(t, map[string][]byte{target: replacement})
				assertNoBackups(t, dstDir)
				return
			}
			if err == nil {
				t.Fatal("installPkg succeeded, want installer failure")
			}
			assertContents(t, map[string][]byte{target: content})
			assertNoBackups(t, dstDir)
			fi, err := os.Stat(target)
			if err != nil {
				t.Fatalf("Stat(%q): %v", target, err)
			}
			if got := fi.Mode().Perm(); got != 0640 {
				t.Errorf("Mode of restored %q = %v, want %v", target, got, os.FileMode(0640))
			}
		})
	}
}

// TestInstallPkg_BackupFallbackCopyFails verifies that when both the rename
// and the copy to a backup fail and the fallback deletes the file, the install
// proceeds and rollback removes the new file, since the original cannot be
// restored.
func TestInstallPkg_BackupFallbackCopyFails(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "app.dll")
	writeFiles(t, map[string][]byte{target: []byte("unrecoverable original")})

	ops := failingOps()
	ops.backup = func(string) (string, error) { return "", errors.New("simulated rename failure") }
	ops.copyBackup = func(string) (string, error) { return "", errors.New("simulated copy failure") }
	ops.removeOrRename = func(filename string) (string, error) { return "", os.Remove(filename) }

	pkgPath := filepath.Join(t.TempDir(), "nocopy_pkg.goo")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{"app.dll": []byte("replacement")})

	ps := &goolib.PkgSpec{Name: "nocopy_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"app.dll": target}}
	if _, err := installPkg(ops, pkgPath, ps, false, false, db); err == nil {
		t.Fatal("installPkg succeeded, want installer failure")
	}
	assertAbsent(t, target)
	assertNoBackups(t, dstDir)
}

// TestInstallPkg_RemovedEmptyDirRecreatedOnRollback verifies that an empty
// directory removed to make room for a package file is recreated with its
// original mode on rollback, and that other replaced files are restored byte
// for byte.
func TestInstallPkg_RemovedEmptyDirRecreatedOnRollback(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dstDir := t.TempDir()
	emptyDir := filepath.Join(dstDir, "config")
	if err := os.Mkdir(emptyDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(emptyDir, 0750); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	other := filepath.Join(dstDir, "other.txt")
	orig := map[string][]byte{other: []byte("original other")}
	writeFiles(t, orig)

	pkgPath := filepath.Join(t.TempDir(), "dir_to_file_pkg.goo")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{
		"config":    []byte("a file where a directory was"),
		"other.txt": []byte("replacement other"),
	})

	ps := &goolib.PkgSpec{Name: "dir_to_file_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"./": dstDir}}
	if _, err := installPkg(failingOps(), pkgPath, ps, false, false, db); err == nil {
		t.Fatal("installPkg succeeded, want installer failure")
	}

	assertContents(t, orig)
	assertNoBackups(t, dstDir)
	fi, err := os.Lstat(emptyDir)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", emptyDir, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%q is not a directory after rollback: mode %v", emptyDir, fi.Mode())
	}
	if got := fi.Mode().Perm(); got != 0750 {
		t.Errorf("Mode of recreated %q = %v, want %v", emptyDir, got, os.FileMode(0750))
	}
	entries, err := os.ReadDir(emptyDir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", emptyDir, err)
	}
	if len(entries) != 0 {
		t.Errorf("Recreated %q has %d entries, want it empty", emptyDir, len(entries))
	}
}

// TestInstallPkg_RemovedEmptyDirReplacedOnSuccess verifies that a successful
// install leaves the package file in place of the removed empty directory.
func TestInstallPkg_RemovedEmptyDirReplacedOnSuccess(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dstDir := t.TempDir()
	emptyDir := filepath.Join(dstDir, "config")
	if err := os.Mkdir(emptyDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	pkgPath := filepath.Join(t.TempDir(), "dir_to_file_ok_pkg.goo")
	content := []byte("a file where a directory was")
	createStressGooArchive(t, pkgPath, nil, map[string][]byte{"config": content})

	ps := &goolib.PkgSpec{Name: "dir_to_file_ok_pkg", Version: "1.0.0@1", Arch: "noarch", Files: map[string]string{"config": emptyDir}}
	if _, err := installPkg(succeedingOps(), pkgPath, ps, false, false, db); err != nil {
		t.Fatalf("installPkg: %v", err)
	}
	assertContents(t, map[string][]byte{emptyDir: content})
}

// TestRenameToBackup verifies that renameToBackup moves the file to a unique
// sibling in the same directory and preserves its content.
func TestRenameToBackup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.bin")
	content := []byte("some content")
	seen := make(map[string]bool)
	for i := 0; i < 3; i++ {
		writeFiles(t, map[string][]byte{path: content})
		bak, err := renameToBackup(path)
		if err != nil {
			t.Fatalf("renameToBackup: %v", err)
		}
		if filepath.Dir(bak) != dir || !strings.HasPrefix(filepath.Base(bak), "file.bin"+backupInfix) {
			t.Errorf("renameToBackup = %q, want sibling named file.bin%s<suffix>", bak, backupInfix)
		}
		if seen[bak] {
			t.Errorf("renameToBackup reused backup name %q", bak)
		}
		seen[bak] = true
		assertAbsent(t, path)
		assertContents(t, map[string][]byte{bak: content})
	}
}

// TestCopyToBackup verifies that copyToBackup copies the file to a unique
// sibling with the same content and mode and leaves the original in place.
func TestCopyToBackup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.bin")
	content := []byte("some content\x00\xff")
	writeFiles(t, map[string][]byte{path: content})
	if err := os.Chmod(path, 0604); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	seen := make(map[string]bool)
	for i := 0; i < 3; i++ {
		bak, err := copyToBackup(path)
		if err != nil {
			t.Fatalf("copyToBackup: %v", err)
		}
		if filepath.Dir(bak) != dir || !strings.HasPrefix(filepath.Base(bak), "file.bin"+backupInfix) {
			t.Errorf("copyToBackup = %q, want sibling named file.bin%s<suffix>", bak, backupInfix)
		}
		if seen[bak] {
			t.Errorf("copyToBackup reused backup name %q", bak)
		}
		seen[bak] = true
		assertContents(t, map[string][]byte{path: content, bak: content})
		fi, err := os.Stat(bak)
		if err != nil {
			t.Fatalf("Stat(%q): %v", bak, err)
		}
		if got := fi.Mode().Perm(); got != 0604 {
			t.Errorf("Mode of %q = %v, want %v", bak, got, os.FileMode(0604))
		}
	}
}

// TestCopyToBackup_MissingFile verifies that copyToBackup fails for a missing
// file and leaves no backup behind.
func TestCopyToBackup_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := copyToBackup(filepath.Join(dir, "missing.bin")); err == nil {
		t.Error("copyToBackup succeeded for a missing file, want an error")
	}
	assertNoBackups(t, dir)
}
