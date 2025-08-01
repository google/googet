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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
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
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}},
	}
	db.WriteStateToDB(s)
	r := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}},
	}
	db.RemovePkg("test2", "")
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		t.Errorf("Unable to fetch packages: %v", err)
	}
	if diff := cmp.Diff(r, pkgs, cmpopts.IgnoreFields(client.PackageState{}, "InstallDate")); diff != "" {
		fmt.Println(diff)
		t.Errorf("GetPackageState did not return expected result, want: %#v, got: %#v", pkgs, s)
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
