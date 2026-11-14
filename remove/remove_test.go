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
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
)

func init() {
	logger.Init("test", true, false, ioutil.Discard)
}

func TestUninstallPkg(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
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

	f, err := oswrap.Create(filepath.Join(src, "test.goo"))
	if err != nil {
		log.Fatal(err)
	}
	defer oswrap.Remove(f.Name())

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	testFile, err := oswrap.Create(filepath.Join(testFolder3, "foo"))
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	fi, err := testFile.Stat()
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
	if _, err := io.Copy(tw, testFile); err != nil {
		t.Fatal(err)
	}

	if err := testFile.Close(); err != nil {
		t.Fatalf("Failed to close test file: %v", err)
	}

	tw.Close()
	gw.Close()
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	d, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("NewDownloader: %v", err)
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	db.WriteStateToDB(client.GooGetState{
		client.PackageState{
			PackageSpec: &goolib.PkgSpec{
				Name: "foo",
			},
			InstalledFiles: map[string]string{
				testFile.Name(): "chksum",
				testFolder:      "",
				testFolder2:     "",
				testFolder3:     "",
				dst:             "",
			},
			LocalPath: f.Name(),
		},
	})
	if err := uninstallPkg(context.Background(), goolib.PackageInfo{Name: "foo"}, RunUninstaller, d, db); err != nil {
		t.Fatalf("Error running uninstallPkg: %v", err)
	}
	newState, err := db.FetchPkgs("")
	if newState != nil {
		t.Errorf("Package not removed from state: %v", newState)
	}
	for _, n := range []string{testFile.Name(), dst} {
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

// removeFixture installs a fake package, "foo" unless the spec passed to
// newRemoveFixture names another, in a fresh database. Its cached
// package is missing and its DownloadURL points at a server that counts
// requests and answers 404, as a repo that no longer has this version would.
// More packages can be installed with add.
type removeFixture struct {
	db       *googetdb.GooDB
	d        *client.Downloader
	srvURL   string
	requests *atomic.Int32
	fakePkg  // foo
}

// fakePkg is a package installed by removeFixture.add: one installed file in
// its own directory, and the path of its cached package.
type fakePkg struct {
	name                 string
	file, dir, localPath string
}

func newRemoveFixture(t *testing.T, spec goolib.PkgSpec, opts ...func(*client.PackageState)) *removeFixture {
	t.Helper()
	settings.Initialize(t.TempDir(), false)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	d, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("NewDownloader: %v", err)
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	f := &removeFixture{db: db, d: d, srvURL: srv.URL, requests: &requests}
	if spec.Name == "" {
		spec.Name = "foo"
	}
	f.fakePkg = f.add(t, spec, opts...)
	return f
}

// add installs a package named spec.Name, arch noarch, version 1.0.0@1. opts
// can change its state before it is written to the database.
func (f *removeFixture) add(t *testing.T, spec goolib.PkgSpec, opts ...func(*client.PackageState)) fakePkg {
	t.Helper()
	dir := filepath.Join(t.TempDir(), spec.Name)
	if err := oswrap.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "app.exe")
	if err := os.WriteFile(file, []byte("app"), 0644); err != nil {
		t.Fatal(err)
	}
	spec.Arch = "noarch"
	spec.Version = "1.0.0@1"
	base := spec.Name + ".noarch.1.0.0@1.goo"
	ps := client.PackageState{
		PackageSpec:    &spec,
		InstalledFiles: map[string]string{file: "chksum", dir: ""},
		LocalPath:      filepath.Join(t.TempDir(), base),
		DownloadURL:    f.srvURL + "/" + base,
		Checksum:       "0000",
	}
	for _, opt := range opts {
		opt(&ps)
	}
	if err := f.db.WriteStateToDB(client.GooGetState{ps}); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}
	return fakePkg{name: spec.Name, file: file, dir: dir, localPath: ps.LocalPath}
}

// uninstall removes the fixture's package, foo unless the spec passed to
// newRemoveFixture named another.
func (f *removeFixture) uninstall(ctx context.Context, mode Mode) error {
	return uninstallPkg(ctx, goolib.PackageInfo{Name: f.name, Arch: "noarch"}, mode, f.d, f.db)
}

func (f *removeFixture) installed(t *testing.T, p fakePkg) bool {
	t.Helper()
	ps, err := f.db.FetchPkg(goolib.PackageInfo{Name: p.name, Arch: "noarch"})
	if err != nil {
		t.Fatalf("FetchPkg(%s): %v", p.name, err)
	}
	return ps.PackageSpec != nil
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := oswrap.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// checkRemoved checks that foo, or the given packages, are gone from the
// database and their installed files are gone from disk.
func (f *removeFixture) checkRemoved(t *testing.T, pkgs ...fakePkg) {
	t.Helper()
	if len(pkgs) == 0 {
		pkgs = []fakePkg{f.fakePkg}
	}
	for _, p := range pkgs {
		if f.installed(t, p) {
			t.Errorf("%s not removed from state", p.name)
		}
		for _, n := range []string{p.file, p.dir} {
			if exists(t, n) {
				t.Errorf("%s was not removed", n)
			}
		}
	}
}

// checkKept checks that foo, or the given packages, are still in the database
// and their installed files are still on disk.
func (f *removeFixture) checkKept(t *testing.T, pkgs ...fakePkg) {
	t.Helper()
	if len(pkgs) == 0 {
		pkgs = []fakePkg{f.fakePkg}
	}
	for _, p := range pkgs {
		if !f.installed(t, p) {
			t.Errorf("%s removed from state, want it kept", p.name)
		}
		if !exists(t, p.file) {
			t.Errorf("%s was removed, want it kept", p.file)
		}
	}
}

func (f *removeFixture) checkNoRequests(t *testing.T) {
	t.Helper()
	if n := f.requests.Load(); n != 0 {
		t.Errorf("made %d download requests, want 0", n)
	}
}

// A package with nothing to run from the package itself is removed without
// downloading it, even when its version is gone from the repo.
func TestUninstallPkgWithoutPackage(t *testing.T) {
	f := newRemoveFixture(t, goolib.PkgSpec{})
	// Point the temp root at a fresh directory, so the directory created for
	// the uninstall log can be looked for afterwards. os.TempDir reads TMPDIR
	// on Unix and TMP, then TEMP, on Windows; the check globs whatever it
	// resolves to, in case Windows reports the path in another form.
	tmp := t.TempDir()
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, tmp)
	}
	pattern := filepath.Join(os.TempDir(), "googet-remove-*")
	if err := f.uninstall(t.Context(), RunUninstaller); err != nil {
		t.Fatalf("uninstallPkg: %v", err)
	}
	f.checkNoRequests(t)
	f.checkRemoved(t)
	left, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("uninstallPkg left %v behind, want nothing", left)
	}
}

// dirNames returns the names of the entries in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// SkipUninstaller removes the files and DB entry without downloading the
// package or running its uninstall script.
func TestUninstallPkgSkipUninstaller(t *testing.T) {
	f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}})
	if err := f.uninstall(t.Context(), SkipUninstaller); err != nil {
		t.Fatalf("uninstallPkg: %v", err)
	}
	f.checkNoRequests(t)
	f.checkRemoved(t)
}

// A package with an uninstall script still needs the package. Each way it can
// be unavailable to the uninstall command returns an error that matches
// ErrPackageUnavailable, names the package and no CLI flag, and removes
// nothing. Only a failed download makes a request.
func TestUninstallPkgPackageUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		opt          func(*client.PackageState)
		wantRequests bool
	}{
		{"download fails", func(*client.PackageState) {}, true},
		{"no DownloadURL", func(ps *client.PackageState) { ps.DownloadURL = "" }, false},
		{"no LocalPath", func(ps *client.PackageState) { ps.LocalPath = ""; ps.UnpackDir = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}}, tc.opt)
			err := f.uninstall(t.Context(), RunUninstaller)
			if !errors.Is(err, ErrPackageUnavailable) {
				t.Fatalf("uninstallPkg error = %v, want ErrPackageUnavailable", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "foo.noarch.1.0.0@1") {
				t.Errorf("error %q does not name foo.noarch.1.0.0@1", msg)
			}
			if strings.Contains(msg, "-skip_uninstaller") || strings.Contains(msg, "-db_only") {
				t.Errorf("error %q names a CLI flag", msg)
			}
			if got := f.requests.Load() > 0; got != tc.wantRequests {
				t.Errorf("download requested = %v, want %v", got, tc.wantRequests)
			}
			if tc.wantRequests {
				// The download error is kept as the cause.
				if !strings.Contains(msg, "404 Not Found") {
					t.Errorf("error %q does not give the download error, the server's 404 Not Found", msg)
				}
			}
			f.checkKept(t)
		})
	}
}

// When the download fails, the download error is wrapped along with
// ErrPackageUnavailable, so callers can still tell why, for example that the
// removal was cancelled.
func TestUninstallPkgPackageUnavailableWrapsCause(t *testing.T) {
	f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := f.uninstall(ctx, RunUninstaller)
	if !errors.Is(err, ErrPackageUnavailable) {
		t.Errorf("uninstallPkg error = %v, want ErrPackageUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("uninstallPkg error = %v, want it to wrap context.Canceled", err)
	}
	f.checkKept(t)
}

// Each mode removes what its doc comment says. foo has an uninstall script and
// a cached package whose checksum doesn't match, so running the uninstaller
// would download it again from a repo that answers 404.
func TestUninstallPkgModes(t *testing.T) {
	for _, tc := range []struct {
		mode                    Mode
		wantErr                 error
		wantRemoved, wantCached bool
		wantFilesKept           bool
	}{
		{mode: RunUninstaller, wantErr: ErrPackageUnavailable, wantFilesKept: true},
		{mode: SkipUninstaller, wantRemoved: true},
		{mode: DBOnly, wantRemoved: true, wantCached: true, wantFilesKept: true},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}})
			if err := os.WriteFile(f.localPath, []byte("stale"), 0644); err != nil {
				t.Fatal(err)
			}
			err := f.uninstall(t.Context(), tc.mode)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("uninstallPkg(%v) = %v, want %v", tc.mode, err, tc.wantErr)
			}
			if tc.wantErr == nil {
				f.checkNoRequests(t)
			}
			if got := !f.installed(t, f.fakePkg); got != tc.wantRemoved {
				t.Errorf("removed from state = %v, want %v", got, tc.wantRemoved)
			}
			if got := exists(t, f.file); got != tc.wantFilesKept {
				t.Errorf("installed file kept = %v, want %v", got, tc.wantFilesKept)
			}
			// RunUninstaller may have rewritten the cache while downloading.
			if tc.wantErr == nil {
				if got := exists(t, f.localPath); got != tc.wantCached {
					t.Errorf("cached package kept = %v, want %v", got, tc.wantCached)
				}
			}
		})
	}
}

// A Mode outside the enum is an error and changes nothing, rather than being
// treated as one of the modes. All rejects it before touching anything, even
// the dependency map it was given.
func TestUninstallPkgInvalidMode(t *testing.T) {
	f := newRemoveFixture(t, goolib.PkgSpec{})
	bar := f.add(t, goolib.PkgSpec{Name: "bar", PkgDependencies: map[string]string{"foo.noarch": "1.0.0@1"}})
	for _, m := range []Mode{-1, 99} {
		if err := f.uninstall(t.Context(), m); err == nil || !strings.Contains(err.Error(), m.String()) {
			t.Errorf("uninstallPkg(%v) = %v, want an error naming %v", m, err, m)
		}
	}
	deps, _, err := EnumerateDeps(goolib.PackageInfo{Name: "foo", Arch: "noarch"}, f.db)
	if err != nil {
		t.Fatal(err)
	}
	want := DepMap{"foo.noarch": {"bar.noarch"}, "bar.noarch": nil}
	if !reflect.DeepEqual(deps, want) {
		t.Fatalf("EnumerateDeps(foo) = %v, want %v", deps, want)
	}
	err = All(t.Context(), goolib.PackageInfo{Name: "foo", Arch: "noarch"}, deps, Mode(99), f.d, f.db)
	if err == nil || !strings.Contains(err.Error(), "Mode(99)") {
		t.Errorf("All(Mode(99)) = %v, want an error naming Mode(99)", err)
	}
	if !reflect.DeepEqual(deps, want) {
		t.Errorf("All(Mode(99)) changed the dependency map to %v, want it left as %v", deps, want)
	}
	f.checkNoRequests(t)
	f.checkKept(t, f.fakePkg, bar)
}

// Removing a package that isn't in the database is an error in every mode,
// rather than a panic on its missing spec, and changes nothing: no download
// is attempted, and the installed package foo is left alone.
func TestAllNotInstalled(t *testing.T) {
	for _, mode := range []Mode{RunUninstaller, SkipUninstaller, DBOnly} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}})
			before, err := f.db.FetchPkgs("")
			if err != nil {
				t.Fatal(err)
			}
			// EnumerateDeps fails for a package that isn't installed, so
			// give All the map it would build: the package alone.
			pi := goolib.PackageInfo{Name: "missing", Arch: "noarch"}
			deps := DepMap{"missing.noarch": nil}
			err = func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("panic: %v", r)
					}
				}()
				return All(t.Context(), pi, deps, mode, f.d, f.db)
			}()
			if err == nil || !strings.Contains(err.Error(), "missing.noarch is not installed") {
				t.Errorf("All(missing, %v) = %v, want an error saying missing.noarch is not installed", mode, err)
			}
			f.checkNoRequests(t)
			f.checkKept(t)
			after, err := f.db.FetchPkgs("")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("All(missing, %v) changed the database from %v to %v", mode, before, after)
			}
		})
	}
}

func TestModeString(t *testing.T) {
	var zero Mode
	if zero != RunUninstaller {
		t.Errorf("zero Mode = %v, want RunUninstaller", zero)
	}
	for _, tc := range []struct {
		mode Mode
		want string
	}{
		{RunUninstaller, "RunUninstaller"},
		{SkipUninstaller, "SkipUninstaller"},
		{DBOnly, "DBOnly"},
		{Mode(99), "Mode(99)"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.mode.String(); got != tc.want {
				t.Errorf("Mode(%d).String() = %q, want %q", int(tc.mode), got, tc.want)
			}
		})
	}
}

// All passes the mode on to dependent packages. foo and its dependent bar
// both have uninstall scripts, and neither package can be downloaded.
func TestAllPassesModeToDependents(t *testing.T) {
	for _, tc := range []struct {
		mode    Mode
		wantErr error
	}{
		{SkipUninstaller, nil},
		{RunUninstaller, ErrPackageUnavailable},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			script := goolib.ExecFile{Path: "uninstall.sh"}
			f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: script})
			bar := f.add(t, goolib.PkgSpec{Name: "bar", Uninstall: script, PkgDependencies: map[string]string{"foo.noarch": "1.0.0@1"}})
			pi := goolib.PackageInfo{Name: "foo", Arch: "noarch"}
			deps, _, err := EnumerateDeps(pi, f.db)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := deps["bar.noarch"]; !ok {
				t.Fatalf("EnumerateDeps(foo) = %v, want bar.noarch in it", deps)
			}
			err = All(t.Context(), pi, deps, tc.mode, f.d, f.db)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("All(%v) = %v, want %v", tc.mode, err, tc.wantErr)
			}
			if tc.wantErr != nil {
				f.checkKept(t)
				return
			}
			f.checkNoRequests(t)
			f.checkRemoved(t, f.fakePkg, bar)
		})
	}
}

// A package installed without a saved LocalPath or UnpackDir, and with no
// uninstall script, is still removed.
func TestUninstallPkgNoLocalPath(t *testing.T) {
	f := newRemoveFixture(t, goolib.PkgSpec{}, func(ps *client.PackageState) {
		ps.LocalPath = ""
		ps.UnpackDir = ""
	})
	if err := f.uninstall(t.Context(), RunUninstaller); err != nil {
		t.Fatalf("uninstallPkg: %v", err)
	}
	f.checkNoRequests(t)
	f.checkRemoved(t)
}

// Only Windows removes an msix package using its install path, so elsewhere
// an msix package with no uninstall script doesn't need the package.
func TestUninstallPkgMsixWithoutPackage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows runs the msix install path to remove it")
	}
	f := newRemoveFixture(t, goolib.PkgSpec{Install: goolib.ExecFile{Path: "app.msix"}})
	if err := f.uninstall(t.Context(), RunUninstaller); err != nil {
		t.Fatalf("uninstallPkg: %v", err)
	}
	f.checkNoRequests(t)
	f.checkRemoved(t)
}

// writeGoo writes a package at path holding the given files, all executable.
func writeGoo(t *testing.T, path string, files map[string]string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(out)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, gw, out} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The uninstall script runs from the package extracted next to the cached
// package, and the extraction directory is removed afterwards whether the
// script succeeds or fails. If it fails, nothing else is removed. This runs
// the script with /bin/sh, so it is Linux only: goolib.Exec runs nothing on
// macOS and picks the interpreter by extension on Windows.
func TestUninstallPkgScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("runs a shell script, which goolib.Exec only supports on Linux")
	}
	for _, tc := range []struct {
		name    string
		exit    int
		wantErr bool
	}{
		{"fails", 1, true},
		{"succeeds", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			cache := t.TempDir()
			goo := filepath.Join(cache, "foo.noarch.1.0.0@1.goo")
			writeGoo(t, goo, map[string]string{
				"uninstall.sh": fmt.Sprintf("#!/bin/sh\necho ran > %q\nexit %d\n", marker, tc.exit),
			})
			f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}}, func(ps *client.PackageState) {
				ps.LocalPath = goo
				ps.Checksum = "" // A local install, so the cache isn't checked.
			})

			err := f.uninstall(t.Context(), RunUninstaller)
			if !exists(t, marker) {
				t.Fatalf("uninstall script did not run (uninstallPkg error: %v)", err)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("uninstallPkg error = %v, want error: %v", err, tc.wantErr)
			}
			if errors.Is(err, ErrPackageUnavailable) {
				t.Errorf("uninstallPkg error = %v, want one that doesn't match ErrPackageUnavailable", err)
			}
			f.checkNoRequests(t)

			left := dirNames(t, cache)
			if tc.wantErr {
				f.checkKept(t)
				if len(left) != 1 || left[0] != filepath.Base(goo) {
					t.Errorf("cache dir holds %v, want only the cached package", left)
				}
				return
			}
			f.checkRemoved(t)
			if len(left) != 0 {
				t.Errorf("cache dir holds %v, want nothing", left)
			}
		})
	}
}

// If the package can't be extracted, the removal fails without removing
// anything, and whatever the extraction left in the cache directory is
// removed. The package is a local install, so the cache isn't checked or
// downloaded again.
func TestUninstallPkgExtractFails(t *testing.T) {
	big := make([]byte, 64<<10)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"not gzip", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not a package"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated", func(t *testing.T, path string) {
			// Incompressible content, so cutting the package in half
			// leaves a partly written file in the extraction directory.
			writeGoo(t, path, map[string]string{"uninstall.sh": "#!/bin/sh\n", "data.bin": string(big)})
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b[:len(b)/2], 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			goo := filepath.Join(cache, "foo.noarch.1.0.0@1.goo")
			tc.write(t, goo)
			f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}}, func(ps *client.PackageState) {
				ps.LocalPath = goo
				ps.Checksum = ""
			})

			err := f.uninstall(t.Context(), RunUninstaller)
			if err == nil {
				t.Fatal("uninstallPkg succeeded, want an extraction error")
			}
			if errors.Is(err, ErrPackageUnavailable) {
				t.Errorf("uninstallPkg error = %v, want one that doesn't match ErrPackageUnavailable", err)
			}
			f.checkNoRequests(t)
			f.checkKept(t)
			if left := dirNames(t, cache); len(left) != 1 || left[0] != filepath.Base(goo) {
				t.Errorf("cache dir holds %v, want only the cached package", left)
			}
		})
	}
}

// A cached package path with no extension would be extracted into itself.
// Extraction fails, and the cached package must not be removed as if it
// were the extraction directory.
func TestUninstallPkgLocalPathWithoutExtension(t *testing.T) {
	cache := t.TempDir()
	pkg := filepath.Join(cache, "foo")
	writeGoo(t, pkg, map[string]string{"uninstall.sh": "#!/bin/sh\n"})
	f := newRemoveFixture(t, goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "uninstall.sh"}}, func(ps *client.PackageState) {
		ps.LocalPath = pkg
		ps.Checksum = ""
	})

	if err := f.uninstall(t.Context(), RunUninstaller); err == nil {
		t.Fatal("uninstallPkg succeeded, want an extraction error")
	}
	f.checkNoRequests(t)
	f.checkKept(t)
	if !exists(t, pkg) {
		t.Errorf("cached package %s was removed, want it kept", pkg)
	}
}
