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

// Package googetdb manages the googet state sqlite database.
package googetdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"

	_ "modernc.org/sqlite" // Import the SQLite driver (unnamed)
)

const (
	stateQuery = `INSERT or REPLACE INTO InstalledPackages (pkg_name, pkg_ver, pkg_arch, pkg_json) VALUES (
		?, ?, ?, ?)`

	// busyTimeoutMS is how long, in milliseconds, a connection retries a lock
	// held by another connection before it fails with SQLITE_BUSY.
	busyTimeoutMS = 5000
	// journalMode is the rollback journal mode of every connection.
	// TRUNCATE empties the journal after a commit instead of deleting it, so
	// the file never disappears under a reader. It is never WAL, because WAL
	// readers write the -shm file.
	journalMode = "TRUNCATE"
)

// nowFunc returns the current time; can be overridden in tests.
var nowFunc = time.Now

// GooDB is the googet database.
type GooDB struct {
	db *sql.DB
}

// NewDB returns the googet DB object
func NewDB(dbFile string) (*GooDB, error) {
	var gdb GooDB
	var err error
	if _, err := os.Stat(dbFile); errors.Is(err, os.ErrNotExist) {
		gdb.db, err = createDB(dbFile)
		if err != nil {
			return nil, err
		}
		return &gdb, nil
	}
	gdb.db, err = sql.Open("sqlite", dsn(dbFile))
	if err != nil {
		return nil, err
	}
	return &gdb, nil
}

// dsn returns the data source name for dbFile. The driver runs
// the query parameters as pragmas on every connection that database/sql opens,
// busy_timeout first, so a pooled connection can't miss them. A DSN without a
// "file:" prefix has its query removed and the rest opened as a plain
// filename, so Windows drive letters and backslashes are passed through
// unchanged.
func dsn(dbFile string) string {
	return fmt.Sprintf("%s?_busy_timeout=%d&_journal_mode=%s", dbFile, busyTimeoutMS, journalMode)
}

// OpenReadOnly opens the existing googet database dbFile for reading only, for
// runs that don't hold the googet lock and may not be elevated. It never
// creates, writes or deletes the database or its journal:
//
//   - If dbFile doesn't exist, it returns an error wrapping os.ErrNotExist
//     without asking SQLite to open anything.
//   - If the file isn't a SQLite database in rollback-journal mode, as
//     googet's own connections leave it, it returns an error without asking
//     SQLite to open it. SQLite creates a WAL database's -wal and -shm files
//     even on a read-only connection.
//   - The connection is opened through a percent-encoded "file:" URI with
//     mode=ro. modernc.org/sqlite opens every connection with
//     SQLITE_OPEN_READWRITE|SQLITE_OPEN_CREATE, and drops the query from a
//     data source name that has no "file:" prefix, so only the URI form makes
//     SQLite open the file read-only and refuse to create a missing one.
//   - query_only(1) makes SQLite refuse every write on the connection too.
//   - It never sets immutable=1, which would make SQLite skip locking and so
//     read a database that an elevated writer is changing.
//
// The connection sets the same busy timeout as read-write connections, so a
// reader waits for an elevated writer's commit instead of failing at once.
// OpenReadOnly reads the database header before it returns, so a file that
// can't be opened or read, or isn't a database, fails here rather than at the
// first query.
func OpenReadOnly(dbFile string) (*GooDB, error) {
	abs, err := filepath.Abs(dbFile)
	if err != nil {
		return nil, fmt.Errorf("opening database %q read-only: %w", dbFile, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("opening database %q read-only: %w", abs, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("opening database %q read-only: is a directory", abs)
	}
	// When SQLite checks an empty database file for a hot journal, it may
	// delete a leftover journal, even on a read-only connection. A googet
	// database is never empty once created, so refuse it instead.
	if fi.Size() == 0 {
		return nil, fmt.Errorf("opening database %q read-only: file is empty", abs)
	}
	if err := checkRollbackHeader(abs); err != nil {
		return nil, fmt.Errorf("opening database %q read-only: %w", abs, err)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(abs))
	if err != nil {
		return nil, fmt.Errorf("opening database %q read-only: %w", abs, err)
	}
	var schemaVersion int64
	if err := db.QueryRowContext(context.Background(), "PRAGMA schema_version").Scan(&schemaVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening database %q read-only: %w", abs, err)
	}
	return &GooDB{db: db}, nil
}

// sqliteMagic is the string that starts the header of every SQLite database
// file.
const sqliteMagic = "SQLite format 3\x00"

// checkRollbackHeader reads the 100-byte header of the database file dbFile
// through oswrap.OpenShared, and returns an error unless the file is a SQLite
// database in rollback-journal mode. Header bytes 18 and 19, the file format
// write and read versions, are 1 for a rollback journal and 2 for WAL. SQLite
// opens a WAL database's -wal and -shm files read-write, and creates them,
// even on a read-only connection, so OpenReadOnly must never hand it one.
// googet's own connections use a rollback journal.
func checkRollbackHeader(dbFile string) error {
	f, err := oswrap.OpenShared(dbFile)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [100]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return fmt.Errorf("reading the database header: %w", err)
	}
	if string(hdr[:len(sqliteMagic)]) != sqliteMagic {
		return errors.New("not a SQLite database")
	}
	if hdr[18] != 1 || hdr[19] != 1 {
		return fmt.Errorf("database isn't in rollback-journal mode (file format versions %d and %d)", hdr[18], hdr[19])
	}
	return nil
}

// readOnlyDSN returns the read-only data source name for the absolute path
// absPath: a "file:" URI with an empty authority and a percent-encoded path, so
// that characters such as '?', '#' and '%' in the path can't end it early.
// SQLite decodes the path, and accepts a Windows drive path in the form
// /C:/dir/googet.db. The driver reads _busy_timeout and _pragma from the same
// query, and SQLite ignores them.
func readOnlyDSN(absPath string) string {
	p := filepath.ToSlash(absPath)
	if !strings.HasPrefix(p, "/") {
		// A Windows drive path, such as C:/ProgramData/GooGet/googet.db.
		p = "/" + p
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("_pragma", "query_only(1)")
	q.Set("_busy_timeout", strconv.Itoa(busyTimeoutMS))
	return "file://" + (&url.URL{Path: p}).EscapedPath() + "?" + q.Encode()
}

// Close will close the db connection
func (g *GooDB) Close() error {
	return g.db.Close()
}

// Create db creates the initial googet database
func createDB(dbFile string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(dbFile))
	if err != nil {
		return nil, err
	}

	createDBQuery := `BEGIN;
	CREATE TABLE IF NOT EXISTS InstalledPackages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pkg_name TEXT NOT NULL,
			pkg_arch TEXT NOT NULL,
			pkg_ver TEXT NOT NULL,
			pkg_json BLOB NOT NULL,
			UNIQUE(pkg_name, pkg_arch) ON CONFLICT REPLACE
		) STRICT;
	COMMIT;
		`

	_, err = db.ExecContext(context.Background(), createDBQuery)
	if err != nil {
		return nil, err
	}

	return db, nil
}

// WriteStateToDB writes new or partial state to the db.
func (g *GooDB) WriteStateToDB(gooState client.GooGetState) error {
	for _, pkgState := range gooState {
		if pkgState.PackageSpec == nil {
			continue
		}
		err := g.AddPkg(pkgState)
		if err != nil {
			return err
		}
	}
	return nil
}

// AddPkg adds a single package to the googet database
func (g *GooDB) AddPkg(pkgState client.PackageState) error {
	spec := pkgState.PackageSpec

	pkgState.InstalledApp.Name, pkgState.InstalledApp.Reg = system.AppAssociation(spec, pkgState.LocalPath)
	if pkgState.InstallDate == 0 {
		pkgState.InstallDate = nowFunc().Unix()
	}

	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	jsonState, err := json.Marshal(pkgState)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(context.Background(), stateQuery, spec.Name, spec.Version, spec.Arch, jsonState)
	if err != nil {
		tx.Rollback()
		return err
	}
	err = tx.Commit()
	if err != nil {
		return err
	}
	return nil
}

// RemovePkg removes a single package from the googet database
func (g *GooDB) RemovePkg(pkgName, arch string) error {
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	query := `DELETE FROM InstalledPackages where pkg_name = ? and pkg_arch = ?`
	if _, err := tx.ExecContext(context.Background(), query, pkgName, arch); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// FetchPkg exports a single package from the googet database
func (g *GooDB) FetchPkg(pi goolib.PackageInfo) (client.PackageState, error) {
	selectSpecQuery :=
		`SELECT 
			pkg_json
		FROM
			InstalledPackages
		WHERE pkg_name = ?
		ORDER BY pkg_arch, pkg_ver
		`
	spec, err := g.db.Query(selectSpecQuery, pi.Name)
	if err != nil {
		return client.PackageState{}, err
	}
	defer spec.Close()

	var lastPkgState client.PackageState
	for spec.Next() {
		var jsonState string
		if err := spec.Scan(&jsonState); err != nil {
			return client.PackageState{}, err
		}
		var pkgState client.PackageState
		if err := json.Unmarshal([]byte(jsonState), &pkgState); err != nil {
			return client.PackageState{}, err
		}
		if pkgState.Match(pi) {
			return pkgState, nil
		}
		lastPkgState = pkgState
	}
	return lastPkgState, nil
}

// FetchPkgs exports all of the current packages in the googet database
func (g *GooDB) FetchPkgs(pkgName string) (client.GooGetState, error) {
	var state client.GooGetState
	query := `SELECT pkg_json FROM InstalledPackages ORDER BY pkg_name, pkg_arch`
	var args []any
	if pkgName != "" {
		query = `SELECT pkg_json FROM InstalledPackages WHERE pkg_name LIKE ? ORDER BY pkg_name, pkg_arch`
		args = []any{pkgName}
	}
	rows, err := g.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var jsonState string
		if err := rows.Scan(&jsonState); err != nil {
			return nil, err
		}
		var pkgState client.PackageState
		if err := json.Unmarshal([]byte(jsonState), &pkgState); err != nil {
			return nil, err
		}
		state = append(state, pkgState)
	}
	return state, nil
}

// InstalledLockState returns the architecture, lock status, and version of the installed package.
// If the package is not installed, it returns empty string and false.
func (g *GooDB) InstalledLockState(pkgName string) (installedArch string, isLocked bool, ver string, pkgFound bool, err error) {
	pkgs, err := g.FetchPkgs(pkgName)
	if err != nil {
		return "", false, "", false, err
	}
	if len(pkgs) == 0 {
		return "", false, "", false, nil
	}
	// Assume only one arch is installed, or pick the first one.
	return pkgs[0].PackageSpec.Arch, pkgs[0].PackageSpec.LockArch, pkgs[0].PackageSpec.Version, true, nil
}

// readState reads the JSON installed package state from the given path,
// retrying with a .bak extension if the first read fails.
//
// Deprecated: Use the googet.db sqlite database instead.
func readState(sf string) (client.GooGetState, error) {
	state, err := readStateFromPath(sf)
	if err != nil {
		sfNotExist := os.IsNotExist(err)
		state, err = readStateFromPath(sf + ".bak")
		if sfNotExist && os.IsNotExist(err) {
			logger.Info("No state file found, assuming no packages installed.")
			return client.GooGetState{}, nil
		}
	}
	return state, err
}

// readStateFromPath is a helper function for readState.
func readStateFromPath(sf string) (client.GooGetState, error) {
	var s client.GooGetState
	b, err := os.ReadFile(sf)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Exists returns true if the database file already exists.
func Exists(dbFile string) bool {
	_, err := os.Stat(dbFile)
	return !errors.Is(err, os.ErrNotExist)
}

// CreateIfMissing creates a new database file at the given path, seeding it
// with the contents of the old JSON state file if it exists.
func CreateIfMissing(dbFile string) error {
	if Exists(dbFile) {
		return nil
	}
	fmt.Println("Creating Googet DB and converting state file...")
	db, err := NewDB(dbFile)
	if err != nil {
		return fmt.Errorf("unable to create initial db file: %v", err)
	}
	defer db.Close()
	// If state file still exists, then convert.
	state, err := readState(settings.StateFile())
	if err != nil {
		return fmt.Errorf("could not read state file: %v", err)
	}
	return db.WriteStateToDB(state)
}
