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
	"os"
	"time"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"

	_ "modernc.org/sqlite" // Import the SQLite driver (unnamed)
)

const (
	stateQuery = `INSERT or REPLACE INTO InstalledPackages (pkg_name, pkg_ver, pkg_arch, pkg_json) VALUES (
		?, ?, ?, ?)`
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
	gdb.db, err = sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, err
	}
	return &gdb, nil
}

// Close will close the db connection
func (g *GooDB) Close() error {
	return g.db.Close()
}

// Create db creates the initial googet database
func createDB(dbFile string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbFile)
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
func (g *GooDB) RemovePkg(packageName, arch string) error {
	removeQuery := fmt.Sprintf(`BEGIN;
	DELETE FROM InstalledPackages where pkg_name = '%v' and pkg_arch = '%v';
	COMMIT;`, packageName, arch)

	_, err := g.db.ExecContext(context.Background(), removeQuery)
	if err != nil {
		return err
	}
	return nil
}

// FetchPkg exports a single package from the googet database
func (g *GooDB) FetchPkg(pkgName string) (client.PackageState, error) {
	var pkgState client.PackageState

	selectSpecQuery :=
		`SELECT 
			pkg_json
		FROM
			InstalledPackages
		WHERE pkg_name = ?
		ORDER BY pkg_name
		`
	spec, err := g.db.Query(selectSpecQuery, pkgName)
	if err != nil {
		return client.PackageState{}, nil
	}
	defer spec.Close()
	for spec.Next() {
		var jsonState string
		err = spec.Scan(
			&jsonState,
		)
		if err != nil {
			return pkgState, err
		}
		err = json.Unmarshal([]byte(jsonState), &pkgState)
		if err != nil {
			return pkgState, err
		}
	}
	return pkgState, nil
}

// FetchPkgs exports all of the current packages in the googet database
func (g *GooDB) FetchPkgs(pkgName string) (client.GooGetState, error) {
	var state client.GooGetState
	pkgQuery := `Select pkg_name from InstalledPackages`
	if pkgName != "" {
		pkgQuery = fmt.Sprintf(`Select pkg_name from InstalledPackages where pkg_name like "%s"`, pkgName)
	}
	pkgs, err := g.db.Query(pkgQuery)
	if err != nil {
		return nil, err
	}
	for pkgs.Next() {
		var pkgName string
		err = pkgs.Scan(&pkgName)
		if err != nil {
			return nil, err
		}
		pkgState, err := g.FetchPkg(pkgName)
		if err != nil {
			return nil, err
		}
		state = append(state, pkgState)
	}

	return state, nil
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
