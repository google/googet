/*
Copyright 2025 Google Inc. All Rights Reserved.
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

package googetdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

func TestConvertStatetoDB(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	db, err := NewDB(statePath)
	if err != nil {
		t.Errorf("Unable to create database: %+v", err)
	}
	defer db.Close()
	s := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}},
	}
	err = db.WriteStateToDB(s)
	if err != nil {
		t.Errorf("Unable to write packages to db: %v", err)
	}
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		t.Errorf("Unable to fetch packages: %v", err)
	}
	if !cmp.Equal(s, pkgs, cmpopts.IgnoreFields(client.PackageState{}, "InstallDate")) {
		t.Errorf("GetPackageState did not return expected result, want: %#v, got: %#v", pkgs, s)
	}
}

func TestRemovePackage(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	db, err := NewDB(statePath)
	if err != nil {
		t.Errorf("Unable to create database: %+v", err)
	}
	defer db.Close()
	s := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}, InstallDate: 123456789},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}, InstallDate: 123456789},
	}
	db.WriteStateToDB(s)
	r := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}, InstallDate: 123456789},
	}
	db.RemovePkg("test2", "")
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		t.Errorf("Unable to fetch packages: %v", err)
	}
	if diff := cmp.Diff(r, pkgs); diff != "" {
		fmt.Println(diff)
		t.Errorf("GetPackageState did not return expected result, want: %#v, got: %#v", pkgs, s)
	}
}

func TestWriteStateToDBPreservesExistingTimestamps(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "googet.db")
	db, err := NewDB(dbFile)
	if err != nil {
		t.Fatalf("NewDB(%v): %v", dbFile, err)
	}
	defer db.Close()
	// Set the initial package state.
	s := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1", Version: "1"}, InstallDate: 123456789},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2", Version: "1"}, InstallDate: 123456789},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test3", Version: "1"}, InstallDate: 123456789},
	}
	if err := db.WriteStateToDB(s); err != nil {
		t.Fatalf("db.WriteStateToDB(%v): %v", s, err)
	}
	// Update the packages in the db.
	nowFunc = func() time.Time { return time.Unix(175000000, 0) }
	t.Cleanup(func() { nowFunc = time.Now })
	s = client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2", Version: "2"}},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test4", Version: "3"}},
	}
	if err := db.WriteStateToDB(s); err != nil {
		t.Fatalf("db.WriteStateToDB(%v): %v", s, err)
	}
	got, err := db.FetchPkgs("")
	if err != nil {
		t.Fatalf("db.FetchPkgs: %v", err)
	}
	want := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1", Version: "1"}, InstallDate: 123456789},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2", Version: "2"}, InstallDate: 175000000},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test3", Version: "1"}, InstallDate: 123456789},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test4", Version: "3"}, InstallDate: 175000000},
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("FetchPkgs got unexpected diff (-want +got):\n%v", diff)
	}
}

func TestFetchPkgsReturnsDistinctEntriesForDifferentArches(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "googet.db")
	db, err := NewDB(dbFile)
	if err != nil {
		t.Fatalf("NewDB(%v): %v", dbFile, err)
	}
	defer db.Close()

	want := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test-pkg", Arch: "arm64", Version: "1"}, InstallDate: 123456789},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test-pkg", Arch: "x86_32", Version: "1"}, InstallDate: 123456789},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test-pkg", Arch: "x86_64", Version: "1"}, InstallDate: 123456789},
	}
	if err := db.WriteStateToDB(want); err != nil {
		t.Fatalf("db.WriteStateToDB(%v): %v", want, err)
	}

	got, err := db.FetchPkgs("")
	if err != nil {
		t.Fatalf("db.FetchPkgs: %v", err)
	}

	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("FetchPkgs got unexpected diff (-want +got):\n%v", diff)
	}

	gotPkg, err := db.FetchPkg(goolib.PackageInfo{Name: "test-pkg", Arch: "x86_32"})
	if err != nil {
		t.Fatalf("db.FetchPkg: %v", err)
	}
	if diff := cmp.Diff(want[1], gotPkg, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("FetchPkg got unexpected diff (-want +got):\n%v", diff)
	}

	// If only name is provided, arm64 should be matched first based on sort order.
	gotPkg, err = db.FetchPkg(goolib.PackageInfo{Name: "test-pkg"})
	if err != nil {
		t.Fatalf("db.FetchPkg: %v", err)
	}
	if diff := cmp.Diff(want[0], gotPkg, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("FetchPkg got unexpected diff (-want +got):\n%v", diff)
	}

	// If no arch matches, the last processed package (x86_64) should be returned.
	gotPkg, err = db.FetchPkg(goolib.PackageInfo{Name: "test-pkg", Arch: "none"})
	if err != nil {
		t.Fatalf("db.FetchPkg: %v", err)
	}
	if diff := cmp.Diff(want[2], gotPkg, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("FetchPkg got unexpected diff (-want +got):\n%v", diff)
	}
}

func TestCreateIfMissing(t *testing.T) {
	for _, tc := range []struct {
		desc      string             // description of test case
		initialDB client.GooGetState // initial contents of db file
		stateFile client.GooGetState // initial contents of state file
		want      client.GooGetState // expected db contents after call
	}{
		{
			desc: "no-db-but-existing-state-file",
			stateFile: client.GooGetState{
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}, InstallDate: 1754021224},
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}, InstallDate: 1735569000},
			},
			want: client.GooGetState{
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}, InstallDate: 1754021224},
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}, InstallDate: 1735569000},
			},
		},
		{
			desc: "existing-db-and-state-file",
			initialDB: client.GooGetState{
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}, InstallDate: 1754021224},
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}, InstallDate: 1735569000},
			},
			stateFile: client.GooGetState{
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "ignore3"}, InstallDate: 1625425200},
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "ignore4"}, InstallDate: 1698717600},
			},
			want: client.GooGetState{
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}, InstallDate: 1754021224},
				client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}, InstallDate: 1735569000},
			},
		},
		{
			desc: "no-db-and-no-state-file",
			want: client.GooGetState{},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			dbFile := settings.DBFile()
			if len(tc.initialDB) > 0 {
				db, err := NewDB(dbFile)
				if err != nil {
					t.Fatalf("NewDB(%v): %v", dbFile, err)
				}
				if err := db.WriteStateToDB(tc.initialDB); err != nil {
					t.Fatalf("WriteStateToDB: %v", err)
				}
				db.Close()
			}
			if len(tc.stateFile) > 0 {
				b, err := json.Marshal(tc.stateFile)
				if err != nil {
					t.Fatalf("json.Marshal(%v): %v", tc.stateFile, err)
				}
				os.WriteFile(settings.StateFile(), b, 0664)
			}
			if err := CreateIfMissing(dbFile); err != nil {
				t.Fatalf("CreateIfMissing(%v): %v", dbFile, err)
			}
			db, err := NewDB(dbFile)
			if err != nil {
				t.Fatalf("NewDB(%v): %v", dbFile, err)
			}
			defer db.Close()
			pkgs, err := db.FetchPkgs("")
			if err != nil {
				t.Fatalf("Unable to fetch packages: %v", err)
			}
			if diff := cmp.Diff(tc.want, pkgs, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("FetchPkgs got unexpected diff (-want +got):\n%v", diff)
			}
		})
	}
}

func TestAddPkgPreservesExecutableNamesAndInstallPath(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "googet.db")
	db, err := NewDB(dbFile)
	if err != nil {
		t.Fatalf("NewDB(%v): %v", dbFile, err)
	}
	defer db.Close()

	ps := client.PackageState{
		PackageSpec: &goolib.PkgSpec{
			Name:            "firefox",
			Arch:            "x86_64",
			Version:         "1.0.0@1",
			ExecutableNames: []string{"firefox.exe"},
			InstallPath:     `C:\Program Files\Mozilla Firefox`,
		},
		InstallDate: 123456789,
	}
	if err := db.AddPkg(ps); err != nil {
		t.Fatalf("db.AddPkg(%v): %v", ps, err)
	}

	pkgs, err := db.FetchPkgs("")
	if err != nil {
		t.Fatalf("db.FetchPkgs: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("got %d packages, want 1", len(pkgs))
	}
	gotSpec := pkgs[0].PackageSpec
	if diff := cmp.Diff(ps.PackageSpec.ExecutableNames, gotSpec.ExecutableNames); diff != "" {
		t.Errorf("ExecutableNames mismatch (-want +got):\n%s", diff)
	}
	if gotSpec.InstallPath != ps.PackageSpec.InstallPath {
		t.Errorf("InstallPath mismatch: got %q, want %q", gotSpec.InstallPath, ps.PackageSpec.InstallPath)
	}
}

func TestAddPkgPreservesURLFields(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "googet.db")
	db, err := NewDB(dbFile)
	if err != nil {
		t.Fatalf("NewDB(%q) failed: %v", dbFile, err)
	}
	defer db.Close()

	ps := client.PackageState{
		PackageSpec: &goolib.PkgSpec{
			Name:       "gimp",
			Arch:       "x86_64",
			Version:    "1.0.0@1",
			Homepage:   "https://www.gimp.org/",
			HelpURL:    "https://docs.gimp.org/",
			Bugtracker: "https://gitlab.gnome.org/GNOME/gimp/-/issues",
		},
		InstallDate: 123456789,
	}
	if err := db.AddPkg(ps); err != nil {
		t.Fatalf("db.AddPkg(%v) failed: %v", ps, err)
	}

	pkgs, err := db.FetchPkgs("")
	if err != nil {
		t.Fatalf("db.FetchPkgs(\"\") failed: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("db.FetchPkgs(\"\") returned %d packages, want 1", len(pkgs))
	}
	if diff := cmp.Diff(ps.PackageSpec, pkgs[0].PackageSpec); diff != "" {
		t.Errorf("db.FetchPkgs(\"\") PackageSpec diff (-want +got):\n%s", diff)
	}
}

// connPragmas returns busy_timeout and journal_mode as seen by each of n
// connections that db holds open at the same time, so each one is a separate
// pooled connection.
func connPragmas(t *testing.T, db *GooDB, n int) (timeouts []int, modes []string) {
	t.Helper()
	ctx := context.Background()
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for range n {
		c, err := db.db.Conn(ctx)
		if err != nil {
			t.Fatalf("db.Conn: %v", err)
		}
		conns = append(conns, c)
		var timeout int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatalf("PRAGMA busy_timeout: %v", err)
		}
		var mode string
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("PRAGMA journal_mode: %v", err)
		}
		timeouts = append(timeouts, timeout)
		modes = append(modes, mode)
	}
	return timeouts, modes
}

func TestConnectionsSetBusyTimeoutAndJournalMode(t *testing.T) {
	const conns = 3
	for _, tc := range []struct {
		desc  string
		setup func(t *testing.T, dbFile string)
	}{
		{
			desc:  "new-db",
			setup: func(*testing.T, string) {},
		},
		{
			desc: "existing-db",
			setup: func(t *testing.T, dbFile string) {
				db, err := NewDB(dbFile)
				if err != nil {
					t.Fatalf("NewDB(%v): %v", dbFile, err)
				}
				db.Close()
			},
		},
		{
			// A database left in WAL mode, for example by another tool, must
			// be switched back, because WAL readers write the -shm file.
			desc: "existing-wal-db",
			setup: func(t *testing.T, dbFile string) {
				raw, err := sql.Open("sqlite", dbFile)
				if err != nil {
					t.Fatalf("sql.Open(%v): %v", dbFile, err)
				}
				defer raw.Close()
				var mode string
				if err := raw.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
					t.Fatalf("PRAGMA journal_mode=WAL = %q, %v; want wal, nil", mode, err)
				}
				if _, err := raw.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
					t.Fatalf("CREATE TABLE: %v", err)
				}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dbFile := filepath.Join(t.TempDir(), "googet.db")
			tc.setup(t, dbFile)
			db, err := NewDB(dbFile)
			if err != nil {
				t.Fatalf("NewDB(%v): %v", dbFile, err)
			}
			defer db.Close()
			timeouts, modes := connPragmas(t, db, conns)
			for i := range conns {
				if timeouts[i] != busyTimeoutMS {
					t.Errorf("connection %d: busy_timeout = %d, want %d", i, timeouts[i], busyTimeoutMS)
				}
				if modes[i] != "truncate" {
					t.Errorf("connection %d: journal_mode = %q, want truncate", i, modes[i])
				}
			}
		})
	}
}

func TestAddPkgWaitsForReader(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "googet.db")
	writer, err := NewDB(dbFile)
	if err != nil {
		t.Fatalf("NewDB(%v): %v", dbFile, err)
	}
	defer writer.Close()
	seed := client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "seed", Arch: "noarch", Version: "1.0.0"}}
	if err := writer.AddPkg(seed); err != nil {
		t.Fatalf("AddPkg(%v): %v", seed.PackageSpec.Name, err)
	}

	reader, err := NewDB(dbFile)
	if err != nil {
		t.Fatalf("NewDB(%v): %v", dbFile, err)
	}
	defer reader.Close()
	// A read inside a transaction holds a SHARED lock until the transaction
	// ends, which stops the writer's COMMIT from taking the EXCLUSIVE lock.
	tx, err := reader.db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM InstalledPackages").Scan(&n); err != nil {
		tx.Rollback()
		t.Fatalf("SELECT count(*): %v", err)
	}
	const hold = 300 * time.Millisecond
	start := time.Now()
	release := time.AfterFunc(hold, func() { tx.Rollback() })
	defer func() {
		if release.Stop() {
			tx.Rollback()
		}
	}()

	ps := client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0"}}
	if err := writer.AddPkg(ps); err != nil {
		t.Fatalf("AddPkg(%v) while a reader holds a read transaction: %v", ps.PackageSpec.Name, err)
	}
	// The write can only finish after the reader lets go, which proves the
	// read transaction really blocked it.
	if elapsed := time.Since(start); elapsed < hold/2 {
		t.Errorf("AddPkg returned after %v, before the reader released its lock after %v", elapsed, hold)
	}
	got, err := writer.FetchPkgs("foo")
	if err != nil {
		t.Fatalf("FetchPkgs(foo): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("FetchPkgs(foo) returned %d packages, want 1", len(got))
	}
}

// readOnlyFixture creates a database with package foo in dir/name, backdates
// it, and returns its path and contents. It builds the database in a plain
// temporary directory and copies it, because NewDB's data source name can't
// hold every character that the read-only tests put in dir.
func readOnlyFixture(t *testing.T, dir, name string) (string, []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "googet.db")
	db, err := NewDB(src)
	if err != nil {
		t.Fatalf("NewDB(%q): %v", src, err)
	}
	if err := db.WriteStateToDB(client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}}); err != nil {
		db.Close()
		t.Fatalf("WriteStateToDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(dir, name)
	if err := os.WriteFile(dbFile, b, 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dbFile, old, old); err != nil {
		t.Fatal(err)
	}
	return dbFile, b
}

func TestOpenReadOnlyMissing(t *testing.T) {
	dir := t.TempDir()
	before := testutil.TreeSnapshot(t, dir)
	for _, p := range []string{
		filepath.Join(dir, "googet.db"),
		filepath.Join(dir, "missing", "googet.db"),
	} {
		db, err := OpenReadOnly(p)
		if err == nil {
			db.Close()
			t.Errorf("OpenReadOnly(%q) = nil error, want one", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("OpenReadOnly(%q) = %v, want an error wrapping os.ErrNotExist", p, err)
		}
	}
	if diff := cmp.Diff(before, testutil.TreeSnapshot(t, dir)); diff != "" {
		t.Errorf("OpenReadOnly on missing files changed the directory (-before +after), want no db, -journal, -wal or -shm file:\n%s", diff)
	}
}

func TestOpenReadOnlyRefusesEmptyAndDirectory(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}
	// A leftover journal must survive: SQLite may delete it when it opens an
	// empty database file.
	if err := os.WriteFile(empty+"-journal", []byte("leftover"), 0644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "dir.db")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	before := testutil.TreeSnapshot(t, dir)
	for _, p := range []string{empty, sub} {
		if db, err := OpenReadOnly(p); err == nil {
			db.Close()
			t.Errorf("OpenReadOnly(%q) = nil error, want one", p)
		}
	}
	if diff := cmp.Diff(before, testutil.TreeSnapshot(t, dir)); diff != "" {
		t.Errorf("OpenReadOnly changed the directory (-before +after):\n%s", diff)
	}
}

func TestOpenReadOnlyNotADatabase(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "googet.db")
	if err := os.WriteFile(p, []byte("this is not an sqlite database, just text padding it out"), 0644); err != nil {
		t.Fatal(err)
	}
	before := testutil.TreeSnapshot(t, dir)
	if db, err := OpenReadOnly(p); err == nil {
		db.Close()
		t.Errorf("OpenReadOnly(non-database) = nil error, want one at open time")
	}
	if diff := cmp.Diff(before, testutil.TreeSnapshot(t, dir)); diff != "" {
		t.Errorf("OpenReadOnly changed the directory (-before +after):\n%s", diff)
	}
}

// TestOpenReadOnlyRefusesWAL checks that OpenReadOnly refuses a database in
// WAL mode before SQLite opens it, because SQLite would create the -wal and
// -shm files even on a read-only connection.
func TestOpenReadOnlyRefusesWAL(t *testing.T) {
	dir := t.TempDir()
	dbFile, _ := readOnlyFixture(t, dir, "googet.db")
	w, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := w.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
		w.Close()
		t.Fatalf("PRAGMA journal_mode=WAL = %q, %v; want \"wal\"", mode, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbFile + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	if b[18] != 2 || b[19] != 2 {
		t.Fatalf("header file format versions = %d, %d after setting WAL mode, want 2, 2", b[18], b[19])
	}
	before := testutil.TreeSnapshot(t, dir)

	if db, err := OpenReadOnly(dbFile); err == nil {
		db.Close()
		t.Error("OpenReadOnly(WAL database) = nil error, want one")
	}
	if diff := cmp.Diff(before, testutil.TreeSnapshot(t, dir)); diff != "" {
		t.Errorf("OpenReadOnly(WAL database) changed the directory (-before +after):\n%s", diff)
	}
}

// TestOpenReadOnlyReadsButNeverWrites checks that a read-only connection
// reads the database, that every write fails, and that the file's bytes and
// mtime and the directory listing don't change. It runs with and without the
// zero-length journal that a TRUNCATE-mode writer leaves after its commit:
// SQLite reads the first byte of that journal, finds none and treats it as
// not hot, and it deletes a leftover journal only next to an empty database,
// which OpenReadOnly refuses, so the journal must stay in place.
func TestOpenReadOnlyReadsButNeverWrites(t *testing.T) {
	for _, tc := range []struct {
		name         string
		emptyJournal bool
	}{{"no journal", false}, {"empty journal", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbFile, want := readOnlyFixture(t, dir, "googet.db")
			if tc.emptyJournal {
				if err := os.WriteFile(dbFile+"-journal", nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			before := testutil.TreeSnapshot(t, dir)

			db, err := OpenReadOnly(dbFile)
			if err != nil {
				t.Fatalf("OpenReadOnly: %v", err)
			}
			got, err := db.FetchPkgs("foo")
			if err != nil || len(got) != 1 {
				t.Errorf("FetchPkgs(foo) = %v, %v; want one package", got, err)
			}
			writes := map[string]func() error{
				"WriteStateToDB": func() error {
					return db.WriteStateToDB(client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "bar", Arch: "noarch", Version: "1"}}})
				},
				"AddPkg": func() error {
					return db.AddPkg(client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "bar", Arch: "noarch", Version: "1"}})
				},
				"RemovePkg": func() error { return db.RemovePkg("foo", "noarch") },
				"raw CREATE TABLE": func() error {
					_, err := db.db.Exec("CREATE TABLE IF NOT EXISTS sneaky (x INTEGER)")
					return err
				},
				"raw PRAGMA query_only=0 then DELETE": func() error {
					if _, err := db.db.Exec("PRAGMA query_only=0"); err != nil {
						return err
					}
					_, err := db.db.Exec("DELETE FROM InstalledPackages")
					return err
				},
			}
			for name, write := range writes {
				if err := write(); err == nil {
					t.Errorf("%s through OpenReadOnly = nil error, want a write error", name)
				}
			}
			if err := db.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}

			b, err := os.ReadFile(dbFile)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b, want) {
				t.Error("database bytes changed through a read-only connection")
			}
			if diff := cmp.Diff(before, testutil.TreeSnapshot(t, dir)); diff != "" {
				t.Errorf("directory or journal changed through a read-only connection (-before +after):\n%s", diff)
			}
		})
	}
}

// TestOpenReadOnlyPaths checks that paths with characters that are special
// in URIs, spaces and non-ASCII characters open the right file read-only.
func TestOpenReadOnlyPaths(t *testing.T) {
	dirs := []string{"with space", "hash#mark", "percent%20sign", "100%", "日本語", "semi;colon=eq&amp"}
	if runtime.GOOS != "windows" {
		dirs = append(dirs, "question?mark")
	}
	for _, d := range dirs {
		t.Run(d, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, d)
			dbFile, _ := readOnlyFixture(t, dir, "googet.db")
			before := testutil.TreeSnapshot(t, parent)
			db, err := OpenReadOnly(dbFile)
			if err != nil {
				t.Fatalf("OpenReadOnly(%q): %v", dbFile, err)
			}
			defer db.Close()
			if got, err := db.FetchPkgs("foo"); err != nil || len(got) != 1 {
				t.Errorf("FetchPkgs(foo) = %v, %v; want one package", got, err)
			}
			if err := db.RemovePkg("foo", "noarch"); err == nil {
				t.Error("RemovePkg through OpenReadOnly = nil error, want a write error")
			}
			if diff := cmp.Diff(before, testutil.TreeSnapshot(t, parent)); diff != "" {
				t.Errorf("OpenReadOnly created or changed files next to the directory (-before +after):\n%s", diff)
			}
		})
	}
	t.Run("relative", func(t *testing.T) {
		dir := t.TempDir()
		readOnlyFixture(t, dir, "googet.db")
		t.Chdir(dir)
		db, err := OpenReadOnly("googet.db")
		if err != nil {
			t.Fatalf("OpenReadOnly(relative): %v", err)
		}
		defer db.Close()
		if got, err := db.FetchPkgs("foo"); err != nil || len(got) != 1 {
			t.Errorf("FetchPkgs(foo) = %v, %v; want one package", got, err)
		}
	})
}

func TestReadOnlyDSN(t *testing.T) {
	type tc struct{ path, wantPrefix, wantPath string }
	cases := []tc{
		{"/var/lib/googet/googet.db", "file:///var/lib/googet/googet.db?", "/var/lib/googet/googet.db"},
		{"/a b/c#d/e%f/g?h/日本.db", "file:///a%20b/c%23d/e%25f/g%3Fh/%E6%97%A5%E6%9C%AC.db?", "/a b/c#d/e%f/g?h/日本.db"},
		{"C:/ProgramData/GooGet/googet.db", "file:///C:/ProgramData/GooGet/googet.db?", "/C:/ProgramData/GooGet/googet.db"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, tc{`C:\ProgramData\Goo Get\googet.db`, "file:///C:/ProgramData/Goo%20Get/googet.db?", "/C:/ProgramData/Goo Get/googet.db"})
	}
	for _, c := range cases {
		got := readOnlyDSN(c.path)
		if !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("readOnlyDSN(%q) = %q, want prefix %q", c.path, got, c.wantPrefix)
			continue
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Errorf("readOnlyDSN(%q) = %q, not a URI: %v", c.path, got, err)
			continue
		}
		if u.Scheme != "file" || u.Host != "" || u.Path != c.wantPath || u.Fragment != "" {
			t.Errorf("readOnlyDSN(%q) parses as scheme %q, host %q, path %q, fragment %q; want file, \"\", %q, \"\"", c.path, u.Scheme, u.Host, u.Path, u.Fragment, c.wantPath)
		}
		q := u.Query()
		if q.Get("mode") != "ro" {
			t.Errorf("readOnlyDSN(%q) mode = %q, want ro", c.path, q.Get("mode"))
		}
		if q.Get("_pragma") != "query_only(1)" {
			t.Errorf("readOnlyDSN(%q) _pragma = %q, want query_only(1)", c.path, q.Get("_pragma"))
		}
		if q.Get("_busy_timeout") != strconv.Itoa(busyTimeoutMS) {
			t.Errorf("readOnlyDSN(%q) _busy_timeout = %q, want %d", c.path, q.Get("_busy_timeout"), busyTimeoutMS)
		}
		if q.Has("immutable") {
			t.Errorf("readOnlyDSN(%q) = %q, must never set immutable", c.path, got)
		}
	}
}

// TestOpenReadOnlyDuringWrite checks that a read-only open succeeds while an
// elevated writer holds an uncommitted write transaction, sees the committed
// data, and doesn't make the writer's commit fail.
func TestOpenReadOnlyDuringWrite(t *testing.T) {
	dir := t.TempDir()
	dbFile, _ := readOnlyFixture(t, dir, "googet.db")
	w, err := NewDB(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx := context.Background()
	conn, err := w.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM InstalledPackages"); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReadOnly(dbFile)
	if err != nil {
		t.Fatalf("OpenReadOnly during an uncommitted write: %v", err)
	}
	defer r.Close()
	if got, err := r.FetchPkgs("foo"); err != nil || len(got) != 1 {
		t.Errorf("FetchPkgs(foo) during an uncommitted delete = %v, %v; want the committed package", got, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Errorf("writer COMMIT with a read-only connection open: %v", err)
	}
	if got, err := r.FetchPkgs("foo"); err != nil || len(got) != 0 {
		t.Errorf("FetchPkgs(foo) after the commit = %v, %v; want no packages", got, err)
	}
}
