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

// Package db manages the googet state sqlite database.
package googetdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/system"

	_ "modernc.org/sqlite" // Import the SQLite driver (unnamed)
)

const (
	stateQuery = `INSERT or REPLACE INTO InstalledPackages (PkgName, PkgVer, PkgArch, PkgJson) VALUES (
		?, ?, ?, ?)`
)

type gooDB struct {
	db *sql.DB
}

// NewDB returns the googet DB object
func NewDB(dbFile string) (*gooDB, error) {
	var gdb gooDB
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

// Create db creates the initial googet database
func createDB(dbFile string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, err
	}

	createDBQuery := `BEGIN;
	CREATE TABLE IF NOT EXISTS InstalledPackages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			PkgName TEXT NOT NULL,
			PkgArch TEXT NOT NULL,
			PkgVer TEXT NOT NULL,
			PkgJson BLOB NOT NULL,
			UNIQUE(PkgName, PkgArch) ON CONFLICT REPLACE
		) STRICT;
	COMMIT;
		`

	_, err = db.ExecContext(context.Background(), createDBQuery)
	if err != nil {
		fmt.Printf("%v", err)
		return nil, err
	}

	return db, nil
}

// WriteStateToDB writes new or partial state to the db.
func (g *gooDB) WriteStateToDB(gooState *client.GooGetState) error {
	for _, pkgState := range *gooState {
		err := g.addPkg(pkgState)
		if err != nil {
			return err
		}
	}
	return nil
}

func (g *gooDB) addPkg(pkgState client.PackageState) error {
	spec := pkgState.PackageSpec
	pkgState.InstalledApp.Name, pkgState.InstalledApp.Reg = system.AppAssociation(spec.Authors, pkgState.LocalPath, spec.Name, filepath.Ext(spec.Install.Path))
	pkgState.InstallDate = int(time.Now().Unix())
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
func (g *gooDB) RemovePkg(packageName, arch string) error {
	removeQuery := fmt.Sprintf(`BEGIN;
	DELETE FROM InstalledPackages where PkgName = '%v' and PkgArch = '%v';
	COMMIT;`, packageName, arch)

	_, err := g.db.ExecContext(context.Background(), removeQuery)
	if err != nil {
		return err
	}
	return nil
}

// FetchPkg exports a single package from the googet database
func (g *gooDB) FetchPkg(pkgName string) (client.PackageState, error) {
	var pkgState client.PackageState

	selectSpecQuery :=
		`SELECT 
			PkgJson
		FROM
			InstalledPackages
		WHERE PkgName = ?
		ORDER BY PkgName
		`
	spec, err := g.db.Query(selectSpecQuery, pkgName)
	defer spec.Close()
	if err != nil {
		fmt.Printf("%v", err)
	}
	for spec.Next() {
		var jsonState string
		err = spec.Scan(
			&jsonState,
		)
		err = json.Unmarshal([]byte(jsonState), &pkgState)
	}
	return pkgState, nil
}

// FetchPkgs exports all of the current packages in the googet database
func (g *gooDB) FetchPkgs() (client.GooGetState, error) {
	var state client.GooGetState

	pkgs, err := g.db.Query(`Select PkgName from InstalledPackages`)
	if err != nil {
		fmt.Printf("%v", err)
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

func processExitCodes(eCodes string) []int {
	e := strings.Split(eCodes, ",")
	var err error
	codes := make([]int, len(e))
	for i, v := range e {
		codes[i], err = strconv.Atoi(v)
		if err != nil {

		}
	}
	return codes
}
