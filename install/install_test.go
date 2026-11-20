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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"maps"
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
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/verify"
	"github.com/google/logger"
)

func init() {
	logger.Init("test", true, false, ioutil.Discard)
}

// testPayload is the content of the test packages, keyed by the slash-separated
// path under the package's payload directory. A nil value is a directory.
var testPayload = map[string][]byte{
	"bin":          nil,
	"bin/tool.exe": []byte("tool binary"),
	"etc":          nil,
	"etc/sub":      nil,
	"etc/sub/conf": []byte("key=value\n"),
	"README":       []byte("read me"),
}

// writeTestPackage writes a .goo package named name.noarch.version under dir
// that installs files (as testPayload describes) to dst, and returns its path.
func writeTestPackage(t *testing.T, dir, name, version, dst string, files map[string][]byte) string {
	t.Helper()
	spec := goolib.PkgSpec{
		Name:    name,
		Version: version,
		Arch:    "noarch",
		Files:   map[string]string{"payload": dst},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	write := func(h *tar.Header, content []byte) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	write(&tar.Header{Name: "payload/", Typeflag: tar.TypeDir, Mode: 0755}, nil)
	for p, content := range files {
		if content == nil {
			write(&tar.Header{Name: "payload/" + p + "/", Typeflag: tar.TypeDir, Mode: 0755}, nil)
			continue
		}
		write(&tar.Header{Name: "payload/" + p, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(content))}, content)
	}
	write(&tar.Header{Name: name + ".pkgspec", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(specJSON))}, specJSON)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".noarch."+version+".goo")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantFiles returns the InstalledFiles map a real install of files to dst
// records: the SHA256 of each file and "" for each directory.
func wantFiles(dst string, files map[string][]byte) map[string]string {
	want := map[string]string{dst: ""}
	for p, content := range files {
		if content == nil {
			want[filepath.Join(dst, filepath.FromSlash(p))] = ""
			continue
		}
		sum := sha256.Sum256(content)
		want[filepath.Join(dst, filepath.FromSlash(p))] = hex.EncodeToString(sum[:])
	}
	return want
}

// putPayload writes files under dst as a real install would.
func putPayload(t *testing.T, dst string, files map[string][]byte) {
	t.Helper()
	for p, content := range files {
		path := filepath.Join(dst, filepath.FromSlash(p))
		if content == nil {
			if err := os.MkdirAll(path, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// newDB returns a database in a fresh googet root.
func newDB(t *testing.T) *googetdb.GooDB {
	t.Helper()
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// setStrictConflicts sets settings.StrictConflicts to v for the duration of t.
func setStrictConflicts(t *testing.T, v bool) {
	t.Helper()
	old := settings.StrictConflicts
	settings.StrictConflicts = v
	t.Cleanup(func() { settings.StrictConflicts = old })
}

// fetchPkg returns the state of the installed noarch package name, failing t
// if it isn't installed.
func fetchPkg(t *testing.T, db *googetdb.GooDB, name string) client.PackageState {
	t.Helper()
	ps, err := db.FetchPkg(goolib.PackageInfo{Name: name, Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(%s): %v", name, err)
	}
	if ps.PackageSpec == nil {
		t.Fatalf("package %s is not installed", name)
	}
	return ps
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
	got, err := installPkg(f.Name(), &ps, false, false, db)
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

	// Test 1: Conflict without force -> Success by default
	fnBlock := makeInstallFunction(srcDir, dstDir, make(map[string]string), false, false, cm)
	errBlock := fnBlock(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errBlock != nil {
		t.Errorf("expected no conflict error by default, got %v", errBlock)
	}

	// Test 2: Conflict with force -> Success
	fnForce := makeInstallFunction(srcDir, dstDir, make(map[string]string), false, true, cm)
	errForce := fnForce(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errForce != nil {
		t.Errorf("expected no error with force, got %v", errForce)
	}

	// Test 3: Conflict without force in strict mode -> Error
	settings.StrictConflicts = true
	defer func() { settings.StrictConflicts = false }()
	fnStrict := makeInstallFunction(srcDir, dstDir, make(map[string]string), false, false, cm)
	errStrict := fnStrict(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errStrict == nil {
		t.Errorf("expected conflict error in strict mode, got nil")
	}

	// Test 4: Conflict with force in strict mode -> Success
	fnStrictForce := makeInstallFunction(srcDir, dstDir, make(map[string]string), false, true, cm)
	errStrictForce := fnStrictForce(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errStrictForce != nil {
		t.Errorf("expected no error with force in strict mode, got %v", errStrictForce)
	}
}

// TestDBOnlyRecordsChecksums checks that a db_only install records the
// payload's checksum for each file and an empty hash for each directory,
// without writing anything to disk.
func TestDBOnlyRecordsChecksums(t *testing.T) {
	db := newDB(t)
	dst := filepath.Join(t.TempDir(), "install")
	goo := writeTestPackage(t, t.TempDir(), "dbonly", "1.0.0@1", dst, testPayload)

	if err := FromDisk(goo, t.TempDir(), true, false, false, db); err != nil {
		t.Fatalf("FromDisk(dbOnly=true) = %v, want nil", err)
	}
	got := fetchPkg(t, db, "dbonly").InstalledFiles
	if want := wantFiles(dst, testPayload); !maps.Equal(got, want) {
		t.Errorf("db_only InstalledFiles = %v, want %v", got, want)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want IsNotExist: a db_only install must not write files", dst, err)
	}
}

// TestDBOnlyVerify checks that, once the payload is on disk, verify passes for
// a db_only row and flags an edited file.
func TestDBOnlyVerify(t *testing.T) {
	db := newDB(t)
	dst := filepath.Join(t.TempDir(), "install")
	goo := writeTestPackage(t, t.TempDir(), "dbonly", "1.0.0@1", dst, testPayload)
	if err := FromDisk(goo, t.TempDir(), true, false, false, db); err != nil {
		t.Fatalf("FromDisk(dbOnly=true) = %v, want nil", err)
	}
	putPayload(t, dst, testPayload)
	ps := fetchPkg(t, db, "dbonly")
	if ok, err := verify.Files(ps); err != nil || !ok {
		t.Errorf("verify.Files() with the payload on disk = %v, %v; want true, nil", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dst, "etc", "sub", "conf"), []byte("key=edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if ok, err := verify.Files(ps); err != nil || ok {
		t.Errorf("verify.Files() after editing a file = %v, %v; want false, nil", ok, err)
	}
}

// TestDBOnlyConflicts checks that db_only rows take part in file-conflict
// checks and that legacy rows with empty checksums don't.
func TestDBOnlyConflicts(t *testing.T) {
	shared := map[string][]byte{"README": []byte("other read me")}
	for _, tc := range []struct {
		desc    string
		legacy  bool // Install the owner as a legacy row with "" checksums.
		strict  bool
		force   bool
		dbOnly  bool // Install the second package db_only, as `googet check` does.
		wantErr bool
	}{
		{desc: "strict", strict: true, wantErr: true},
		{desc: "strict forced", strict: true, force: true},
		{desc: "not strict", strict: false},
		{desc: "not strict forced", strict: false, force: true},
		{desc: "strict db_only, as googet check", strict: true, dbOnly: true, wantErr: true},
		{desc: "strict forced db_only", strict: true, force: true, dbOnly: true},
		{desc: "strict against legacy row", legacy: true, strict: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			db := newDB(t)
			setStrictConflicts(t, tc.strict)
			dst := filepath.Join(t.TempDir(), "install")
			if tc.legacy {
				legacy := map[string]string{}
				for f := range wantFiles(dst, testPayload) {
					legacy[f] = ""
				}
				if err := db.AddPkg(client.PackageState{
					PackageSpec:    &goolib.PkgSpec{Name: "owner", Version: "1.0.0@1", Arch: "noarch"},
					InstalledFiles: legacy,
				}); err != nil {
					t.Fatalf("db.AddPkg: %v", err)
				}
			} else {
				goo := writeTestPackage(t, t.TempDir(), "owner", "1.0.0@1", dst, testPayload)
				if err := FromDisk(goo, t.TempDir(), true, false, false, db); err != nil {
					t.Fatalf("FromDisk(owner, dbOnly=true) = %v, want nil", err)
				}
			}
			ownerBefore := fetchPkg(t, db, "owner").InstalledFiles

			goo := writeTestPackage(t, t.TempDir(), "other", "1.0.0@1", dst, shared)
			err := FromDisk(goo, t.TempDir(), tc.dbOnly, tc.force, false, db)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "file conflict") || !strings.Contains(err.Error(), "owned by package owner") {
					t.Errorf("FromDisk(other) = %v, want a file conflict with package owner", err)
				}
			} else if err != nil {
				t.Errorf("FromDisk(other) = %v, want nil", err)
			}
			if got := fetchPkg(t, db, "owner").InstalledFiles; !maps.Equal(got, ownerBefore) {
				t.Errorf("owner's InstalledFiles changed from %v to %v", ownerBefore, got)
			}
		})
	}
}

// TestDBOnlyRemove checks that a normal removal of a db_only package deletes
// its files and directories.
func TestDBOnlyRemove(t *testing.T) {
	db := newDB(t)
	dst := filepath.Join(t.TempDir(), "install")
	goo := writeTestPackage(t, t.TempDir(), "dbonly", "1.0.0@1", dst, testPayload)
	if err := FromDisk(goo, t.TempDir(), true, false, false, db); err != nil {
		t.Fatalf("FromDisk(dbOnly=true) = %v, want nil", err)
	}
	putPayload(t, dst, testPayload)
	pi := goolib.PackageInfo{Name: "dbonly", Arch: "noarch"}
	deps, _, err := remove.EnumerateDeps(pi, db)
	if err != nil {
		t.Fatalf("remove.EnumerateDeps: %v", err)
	}
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	if err := remove.All(context.Background(), pi, deps, false, downloader, db); err != nil {
		t.Fatalf("remove.All() = %v, want nil", err)
	}
	for f := range wantFiles(dst, testPayload) {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("os.Stat(%s) after removal = %v, want IsNotExist", f, err)
		}
	}
	if ps, err := db.FetchPkg(pi); err != nil || ps.PackageSpec != nil {
		t.Errorf("db.FetchPkg after removal = %v, %v; want no package", ps.PackageSpec, err)
	}
}
