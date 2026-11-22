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

package verify

import (
	"bytes"
	"database/sql"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

// logs receives everything the package logger writes, so tests can check what
// Execute reported.
var logs bytes.Buffer

func TestMain(m *testing.M) {
	logger.Init("GooGet", false, false, &logs)
	os.Exit(m.Run())
}

func TestExecuteNotInstalled(t *testing.T) {
	for _, tc := range []struct {
		desc  string
		state client.GooGetState
		// corrupt lists packages whose InstalledPackages row holds invalid
		// pkg_json, so FetchPkg fails for them.
		corrupt  []string
		args     []string
		wantLogs []string
	}{
		{
			desc:     "empty-db",
			args:     []string{"A"},
			wantLogs: []string{`Package "A" not installed, cannot verify.`},
		},
		{
			desc: "other-package-installed",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"}},
			},
			args:     []string{"A"},
			wantLogs: []string{`Package "A" not installed, cannot verify.`},
		},
		{
			desc: "multiple-not-installed",
			args: []string{"A", "C"},
			wantLogs: []string{
				`Package "A" not installed, cannot verify.`,
				`Package "C" not installed, cannot verify.`,
			},
		},
		{
			desc:    "lookup-error",
			corrupt: []string{"A"},
			args:    []string{"A", "C"},
			wantLogs: []string{
				`Unable to look up package "A": `,
				`Package "C" not installed, cannot verify.`,
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			if err := db.WriteStateToDB(tc.state); err != nil {
				t.Fatalf("db.WriteStateToDB: %v", err)
			}
			db.Close()
			if len(tc.corrupt) > 0 {
				insertCorruptRows(t, settings.DBFile(), tc.corrupt)
			}
			logs.Reset()

			cmd := &verifyCmd{}
			fs := flag.NewFlagSet("verify", flag.ContinueOnError)
			cmd.SetFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("fs.Parse(%v): %v", tc.args, err)
			}
			if got := cmd.Execute(t.Context(), fs); got != subcommands.ExitFailure {
				t.Errorf("Execute(%v) = %v, want %v", tc.args, got, subcommands.ExitFailure)
			}
			for _, want := range tc.wantLogs {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("Execute(%v) logs = %q, want them to contain %q", tc.args, logs.String(), want)
				}
			}
		})
	}
}

// insertCorruptRows adds an InstalledPackages row with invalid pkg_json for
// each of names to the database in dbFile.
func insertCorruptRows(t *testing.T, dbFile string, names []string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dbFile, err)
	}
	defer db.Close()
	for _, name := range names {
		if _, err := db.Exec(`INSERT INTO InstalledPackages (pkg_name, pkg_arch, pkg_ver, pkg_json) VALUES (?, 'noarch', '1', ?)`, name, []byte("not json")); err != nil {
			t.Fatalf("inserting corrupt row for %q: %v", name, err)
		}
	}
}
