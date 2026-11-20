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
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
)

// testLog receives the log output of the tests.
var testLog syncBuffer

func init() {
	logger.Init("test", true, false, &testLog)
}

// syncBuffer is a bytes.Buffer that is safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// take returns the buffered output and empties the buffer.
func (b *syncBuffer) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	b.buf.Reset()
	return s
}

// captureStdout returns what f writes to os.Stdout.
func captureStdout(t *testing.T, f func()) (out string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		r.Close()
		done <- string(b)
	}()
	orig := os.Stdout
	os.Stdout = w
	// The deferred call also restores os.Stdout if f calls t.Fatal.
	defer func() {
		os.Stdout = orig
		w.Close()
		out = <-done
	}()
	f()
	return ""
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
	got, err := installPkg(f.Name(), &ps, false, false, db, nil)
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

// tree returns the files and directories under root, mapping each slash-
// separated relative path to its contents, or to "/" for a directory.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			got[filepath.ToSlash(rel)] = "/"
			return nil
		}
		b, err := os.ReadFile(path)
		got[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return got
}

// writeGoo writes a package archive at path holding files, which maps
// slash-separated names to contents. Names ending in .sh are executable.
func writeGoo(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for name, body := range files {
		mode := int64(0644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, gw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstallPkgRollback(t *testing.T) {
	pkgFiles := map[string]string{"keep": "new", "sub/added": "added"}
	before := map[string]string{"keep": "old"}
	for _, tc := range []struct {
		desc      string
		files     map[string]string // Package Files, with destinations relative to dst.
		installer string            // A missing installer makes the install fail.
		want      map[string]string // The tree of dst after the install; nil means before.
	}{
		{
			desc:  "success",
			files: map[string]string{"./": "."},
			want:  map[string]string{"keep": "new", "sub": "/", "sub/added": "added"},
		},
		{
			desc:  "success with new parent directories",
			files: map[string]string{"keep": "p/q/keep"},
			want:  map[string]string{"keep": "old", "p": "/", "p/q": "/", "p/q/keep": "new"},
		},
		{
			desc:      "installer fails",
			files:     map[string]string{"./": "."},
			installer: "missing-installer",
		},
		{
			// The second mapping writes sub/added again after the first
			// created it; its original state is still "missing".
			desc:      "installer fails after a path is written twice",
			files:     map[string]string{"sub": "sub", "sub/added": "sub/added"},
			installer: "missing-installer",
		},
		{
			desc:      "installer fails after creating parent directories",
			files:     map[string]string{"./": "a/b", "keep": "p/q/keep"},
			installer: "missing-installer",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			dst := t.TempDir()
			for name, body := range before {
				if err := os.WriteFile(filepath.Join(dst, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			pkg := filepath.Join(t.TempDir(), "test.goo")
			writeGoo(t, pkg, pkgFiles)
			ps := goolib.PkgSpec{Files: map[string]string{}, Install: goolib.ExecFile{Path: tc.installer}}
			for src, rel := range tc.files {
				ps.Files[src] = filepath.Join(dst, rel)
			}

			_, err = installPkg(pkg, &ps, false, false, db, nil)
			want := tc.want
			if want == nil {
				want = before
			}
			if gotErr, wantErr := err != nil, tc.installer != ""; gotErr != wantErr {
				t.Errorf("installPkg(%q) = %v, want error %t", pkg, err, wantErr)
			}
			if tc.installer != "" {
				// system.Install creates its log (googet_install.log, or
				// <installer>.log on Windows) in the extraction directory
				// before it runs the installer, which happens only after
				// every file is copied.
				extracted := strings.TrimSuffix(pkg, filepath.Ext(pkg))
				if logs, _ := filepath.Glob(filepath.Join(extracted, "*.log")); len(logs) == 0 {
					t.Errorf("installPkg(%q) = %v before running the installer, want installer failure", pkg, err)
				}
			}
			if diff := cmp.Diff(want, tree(t, dst)); diff != "" {
				t.Errorf("installPkg(%q) left an unexpected tree in dst (-want +got):\n%s", pkg, diff)
			}
			if tc.installer == "" && runtime.GOOS != "windows" {
				// The umask applies to ref as it does to the directories
				// that the install creates.
				ref := filepath.Join(t.TempDir(), "ref")
				if err := os.Mkdir(ref, 0755); err != nil {
					t.Fatal(err)
				}
				for rel, body := range want {
					if body != "/" {
						continue
					}
					if got, wantPerm := perm(t, filepath.Join(dst, rel)), perm(t, ref); got != wantPerm {
						t.Errorf("installPkg(%q) created %q with mode %v, want %v", pkg, rel, got, wantPerm)
					}
				}
			}
		})
	}
}

// errAddPkg is the error that the addPkg stub returns or panics with.
var errAddPkg = errors.New("injected AddPkg failure")

// addPkgFault is how the addPkg stub fails, if at all.
type addPkgFault int

const (
	addPkgOK addPkgFault = iota
	addPkgError
	addPkgPanic
)

// TestInstallAddPkg checks that FromDisk and FromRepo roll back the
// files of an upgrade when recording it in the database fails or panics, and
// that on success they record it and FromRepo cleans up the old version. A
// dbOnly install changes no installed files either way. The completion
// message is printed only after the install is recorded.
func TestInstallAddPkg(t *testing.T) {
	before := map[string]string{"keep": "old", "oldonly": "old"}
	// The tree when the new files are placed, the backup of the overwritten
	// file is not yet deleted and the old version is not yet cleaned up. The
	// random suffix of the backup name is dropped.
	placedTree := map[string]string{"keep": "new", "keep" + backupInfix: "old", "oldonly": "old", "sub": "/", "sub/added": "added"}
	for _, tc := range []struct {
		desc      string
		fromRepo  bool
		dbOnly    bool
		installer bool // The package has an install script that succeeds.
		fault     addPkgFault
		want      map[string]string
	}{
		{desc: "FromDisk success", want: map[string]string{"keep": "new", "oldonly": "old", "sub": "/", "sub/added": "added"}},
		{desc: "FromDisk error", fault: addPkgError, want: before},
		{desc: "FromDisk error after the installer ran", installer: true, fault: addPkgError, want: before},
		{desc: "FromDisk panic", fault: addPkgPanic, want: before},
		{desc: "FromRepo success", fromRepo: true, want: map[string]string{"keep": "new", "sub": "/", "sub/added": "added"}},
		{desc: "FromRepo error", fromRepo: true, fault: addPkgError, want: before},
		{desc: "FromRepo panic", fromRepo: true, fault: addPkgPanic, want: before},
		{desc: "FromDisk dbOnly success", dbOnly: true, want: before},
		{desc: "FromDisk dbOnly error", dbOnly: true, fault: addPkgError, want: before},
		{desc: "FromRepo dbOnly success", fromRepo: true, dbOnly: true, want: before},
		{desc: "FromRepo dbOnly error", fromRepo: true, dbOnly: true, fault: addPkgError, want: before},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.installer && runtime.GOOS != "linux" {
				// goolib.Exec runs shell scripts only on Linux. On Windows,
				// a successful installer also adds an uninstall entry under
				// HKLM.
				t.Skip("install scripts run only on Linux")
			}
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			dst, cache := t.TempDir(), t.TempDir()
			for name, body := range before {
				if err := os.WriteFile(filepath.Join(dst, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			oldGoo := filepath.Join(cache, "pkg.noarch.1.0.0@1.goo")
			if err := os.WriteFile(oldGoo, []byte("old package"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := db.AddPkg(client.PackageState{
				LocalPath:   oldGoo,
				PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1.0.0@1"},
				InstalledFiles: map[string]string{
					filepath.Join(dst, "keep"):    goolib.Checksum(strings.NewReader("old")),
					filepath.Join(dst, "oldonly"): goolib.Checksum(strings.NewReader("old")),
				},
			}); err != nil {
				t.Fatalf("db.AddPkg: %v", err)
			}

			spec := &goolib.PkgSpec{
				Name:    "pkg",
				Arch:    "noarch",
				Version: "2.0.0@1",
				Files: map[string]string{
					"keep":      filepath.Join(dst, "keep"),
					"sub/added": filepath.Join(dst, "sub", "added"),
				},
			}
			files := map[string]string{"keep": "new", "sub/added": "added"}
			if tc.installer {
				spec.Install = goolib.ExecFile{Path: "install.sh"}
				files["install.sh"] = "#!/bin/sh\n"
			}
			b, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			files["pkg.pkgspec"] = string(b)
			pkg := filepath.Join(t.TempDir(), "pkg.goo")
			writeGoo(t, pkg, files)

			var placed map[string]string
			orig := addPkg
			t.Cleanup(func() { addPkg = orig })
			addPkg = func(db *googetdb.GooDB, ps client.PackageState) error {
				placed = make(map[string]string)
				for rel, body := range tree(t, dst) {
					if i := strings.Index(rel, backupInfix); i >= 0 {
						rel = rel[:i+len(backupInfix)]
					}
					placed[rel] = body
				}
				switch tc.fault {
				case addPkgError:
					return errAddPkg
				case addPkgPanic:
					panic(errAddPkg)
				}
				return orig(db, ps)
			}

			var panicked any
			var install func() error
			if tc.fromRepo {
				goo, err := os.ReadFile(pkg)
				if err != nil {
					t.Fatal(err)
				}
				rs := goolib.RepoSpec{Checksum: goolib.Checksum(bytes.NewReader(goo)), Source: "pkg.goo", PackageSpec: spec}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.ServeFile(w, r, pkg)
				}))
				defer srv.Close()
				repo := srv.URL + "/repo"
				downloader, err := client.NewDownloader("")
				if err != nil {
					t.Fatal(err)
				}
				install = func() error {
					return FromRepo(t.Context(),
						goolib.PackageInfo{Name: "pkg", Arch: "noarch", Ver: "2.0.0@1"},
						repo, cache,
						client.RepoMap{repo: {Packages: []goolib.RepoSpec{rs}}},
						[]string{"noarch"}, tc.dbOnly, false, downloader, db)
				}
			} else {
				install = func() error {
					return FromDisk(pkg, cache, tc.dbOnly, false, false, db)
				}
			}
			testLog.take()
			out := captureStdout(t, func() {
				err = func() error {
					defer func() { panicked = recover() }()
					return install()
				}()
			})
			logs := testLog.take()

			// The files are placed, the backup is kept and the old version is
			// not yet cleaned up by the time the install is recorded. A dbOnly
			// install places no files.
			wantPlaced := placedTree
			if tc.dbOnly {
				wantPlaced = before
			}
			if diff := cmp.Diff(wantPlaced, placed); diff != "" {
				t.Errorf("tree when recording the install (-want +got):\n%s", diff)
			}
			switch tc.fault {
			case addPkgError:
				if !errors.Is(err, errAddPkg) {
					t.Errorf("install = %v, want %v", err, errAddPkg)
				}
			case addPkgPanic:
				if panicked != errAddPkg {
					t.Errorf("install panicked with %v, want %v", panicked, errAddPkg)
				}
			default:
				if err != nil || panicked != nil {
					t.Errorf("install = %v, panic %v, want success", err, panicked)
				}
			}
			if got, want := strings.Contains(out, "completed"), tc.fault == addPkgOK; got != want {
				t.Errorf("install printed a completion message = %t, want %t; output:\n%s", got, want, out)
			}
			// The log says that the installer's changes stay only if an
			// installer ran.
			wantLog := ""
			if tc.fault == addPkgError {
				wantLog = "Recording pkg.noarch.2.0.0@1 in the database failed: " + errAddPkg.Error()
				if tc.installer {
					wantLog = "Recording pkg.noarch.2.0.0@1 in the database failed after its installer ran; rolling back its files, but not the changes made by the installer: " + errAddPkg.Error()
				}
			}
			if got, want := strings.Contains(logs, "in the database failed"), wantLog != ""; got != want || !strings.Contains(logs, wantLog) {
				t.Errorf("install log mentions a database failure = %t, want %t with %q; log:\n%s", got, want, wantLog, logs)
			}
			if strings.Contains(logs, "Rollback:") {
				t.Errorf("install logged a rollback problem; log:\n%s", logs)
			}
			if diff := cmp.Diff(tc.want, tree(t, dst)); diff != "" {
				t.Errorf("tree after the install (-want +got):\n%s", diff)
			}
			_, err = os.Stat(oldGoo)
			if gotOld, wantOld := err == nil, !tc.fromRepo || tc.fault != addPkgOK; gotOld != wantOld {
				t.Errorf("old package %s exists = %t (%v), want %t", oldGoo, gotOld, err, wantOld)
			}
			st, err := db.FetchPkg(goolib.PackageInfo{Name: "pkg"})
			if err != nil {
				t.Fatalf("db.FetchPkg: %v", err)
			}
			wantVer := "2.0.0@1"
			if tc.fault != addPkgOK {
				wantVer = "1.0.0@1"
			}
			if st.PackageSpec.Version != wantVer {
				t.Errorf("installed version = %q, want %q", st.PackageSpec.Version, wantVer)
			}
			// The checksums that a dbOnly install records are not checked.
			if tc.fault == addPkgOK && !tc.dbOnly {
				wantFiles := map[string]string{
					filepath.Join(dst, "keep"):         goolib.Checksum(strings.NewReader("new")),
					filepath.Join(dst, "sub", "added"): goolib.Checksum(strings.NewReader("added")),
				}
				if diff := cmp.Diff(wantFiles, st.InstalledFiles); diff != "" {
					t.Errorf("recorded installed files (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestReinstallRollback checks that Reinstall, which records nothing in the
// database, deletes the backups when it succeeds and rolls the files back
// exactly once when the installer fails.
func TestReinstallRollback(t *testing.T) {
	before := map[string]string{"keep": "old"}
	for _, tc := range []struct {
		desc      string
		installer string // A missing installer makes the reinstall fail.
		want      map[string]string
	}{
		{desc: "success", want: map[string]string{"keep": "new", "sub": "/", "sub/added": "added"}},
		{desc: "installer fails", installer: "missing-installer", want: before},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			dst := t.TempDir()
			for name, body := range before {
				if err := os.WriteFile(filepath.Join(dst, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			pkg := filepath.Join(t.TempDir(), "pkg.goo")
			writeGoo(t, pkg, map[string]string{"keep": "new", "sub/added": "added"})
			ps := client.PackageState{
				LocalPath: pkg,
				PackageSpec: &goolib.PkgSpec{
					Name:    "pkg",
					Arch:    "noarch",
					Version: "1.0.0@1",
					Files: map[string]string{
						"keep":      filepath.Join(dst, "keep"),
						"sub/added": filepath.Join(dst, "sub", "added"),
					},
					Install: goolib.ExecFile{Path: tc.installer},
				},
			}

			testLog.take()
			out := captureStdout(t, func() {
				err = Reinstall(t.Context(), ps, false, false, nil, db)
			})
			logs := testLog.take()

			if gotErr, wantErr := err != nil, tc.installer != ""; gotErr != wantErr {
				t.Errorf("Reinstall() = %v, want error %t", err, wantErr)
			}
			if tc.installer != "" {
				// system.Install creates its log in the extraction directory
				// just before it runs the installer.
				extracted := strings.TrimSuffix(pkg, filepath.Ext(pkg))
				if installerLogs, _ := filepath.Glob(filepath.Join(extracted, "*.log")); len(installerLogs) == 0 {
					t.Errorf("Reinstall() = %v before running the installer, want installer failure", err)
				}
			}
			if got, want := strings.Contains(out, "completed"), tc.installer == ""; got != want {
				t.Errorf("Reinstall() printed a completion message = %t, want %t; output:\n%s", got, want, out)
			}
			// A second rollback would remove the restored file and then fail
			// to restore it from the backup that is already gone.
			if strings.Contains(logs, "Rollback:") || strings.Contains(logs, "in the database failed") {
				t.Errorf("Reinstall() logged an unexpected problem; log:\n%s", logs)
			}
			if diff := cmp.Diff(tc.want, tree(t, dst)); diff != "" {
				t.Errorf("tree after Reinstall() (-want +got):\n%s", diff)
			}
			pkgs, err := db.FetchPkgs("")
			if err != nil {
				t.Fatalf("db.FetchPkgs: %v", err)
			}
			if len(pkgs) != 0 {
				t.Errorf("Reinstall() recorded %v in the database, want nothing", pkgs)
			}
		})
	}
}

func TestUndo(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		commit bool
		want   map[string]string
	}{
		{desc: "rollback", want: map[string]string{"old": "old"}},
		{desc: "commit", commit: true, want: map[string]string{"old": "new", "empty": "new", "a": "/", "a/b": "/", "a/b/new": "new"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "old"), []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "empty"), 0755); err != nil {
				t.Fatal(err)
			}
			var u undo
			write := func(rel string) {
				t.Helper()
				p := filepath.Join(root, rel)
				if err := u.mkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatalf("mkdirAll(%q) = %v", filepath.Dir(p), err)
				}
				if err := u.prepare(p); err != nil {
					t.Fatalf("prepare(%q) = %v", p, err)
				}
				if err := os.WriteFile(p, []byte("new"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			// Each path is written twice; the second write must not replace
			// the record of its original state.
			for _, rel := range []string{"old", "a/b/new", "old", "a/b/new", "empty"} {
				write(rel)
			}
			if tc.commit {
				u.commit()
			} else {
				// The empty directory that a file replaced is not recreated.
				u.rollback()
			}
			if diff := cmp.Diff(tc.want, tree(t, root)); diff != "" {
				t.Errorf("tree after %s (-want +got):\n%s", tc.desc, diff)
			}
		})
	}
}

// perm returns the permission bits of path.
func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestUndoMkdirAllModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not report Unix permission bits")
	}
	root := t.TempDir()
	// The umask applies to the reference directories as it does to the
	// directories that mkdirAll creates.
	refParent, refDir := filepath.Join(root, "ref-parent"), filepath.Join(root, "ref-dir")
	for ref, mode := range map[string]os.FileMode{refParent: 0755, refDir: 0500} {
		if err := os.Mkdir(ref, mode); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(root, "a")
	dir := filepath.Join(parent, "b")
	var u undo
	if err := u.mkdirAll(dir, 0500); err != nil {
		t.Fatalf("mkdirAll(%q, 0500) = %v", dir, err)
	}
	for path, ref := range map[string]string{parent: refParent, dir: refDir} {
		if got, want := perm(t, path), perm(t, ref); got != want {
			t.Errorf("mkdirAll(%q, 0500) created %q with mode %v, want %v", dir, path, got, want)
		}
	}
}

func TestUndoMkdirAllExisting(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	// top is the ancestor of file just below the volume root, such as /tmp or
	// C:\Users. On Windows, oswrap.MkdirAll panics on a volume root, so
	// mkdirAll must not call it on an existing directory or its parent.
	top := file
	for filepath.Dir(filepath.Dir(top)) != filepath.Dir(top) {
		top = filepath.Dir(top)
	}
	for _, tc := range []struct {
		dir     string
		wantErr bool
	}{
		{dir: filepath.Dir(top)},
		{dir: top},
		{dir: filepath.Dir(file)},
		{dir: file, wantErr: true},
	} {
		var u undo
		if err := u.mkdirAll(tc.dir, 0755); (err != nil) != tc.wantErr {
			t.Errorf("mkdirAll(%q) = %v, want error %t", tc.dir, err, tc.wantErr)
		}
		if len(u.changes) != 0 {
			t.Errorf("mkdirAll(%q) recorded %v, want no changes", tc.dir, u.changes)
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
	fnBlock := makeInstallFunction(srcDir, dstDir, make(map[string]string), false, false, cm, &undo{})
	errBlock := fnBlock(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errBlock != nil {
		t.Errorf("expected no conflict error by default, got %v", errBlock)
	}

	// Test 2: Conflict with force -> Success
	fnForce := makeInstallFunction(srcDir, dstDir, make(map[string]string), false, true, cm, &undo{})
	errForce := fnForce(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errForce != nil {
		t.Errorf("expected no error with force, got %v", errForce)
	}

	// Test 3: Conflict without force in strict mode -> Error
	settings.StrictConflicts = true
	defer func() { settings.StrictConflicts = false }()
	fnStrict := makeInstallFunction(srcDir, dstDir, make(map[string]string), false, false, cm, &undo{})
	errStrict := fnStrict(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errStrict == nil {
		t.Errorf("expected conflict error in strict mode, got nil")
	}

	// Test 4: Conflict with force in strict mode -> Success
	fnStrictForce := makeInstallFunction(srcDir, dstDir, make(map[string]string), false, true, cm, &undo{})
	errStrictForce := fnStrictForce(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errStrictForce != nil {
		t.Errorf("expected no error with force in strict mode, got %v", errStrictForce)
	}
}
