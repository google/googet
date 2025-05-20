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
	"fmt"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

func TestConvertStatetoDB(t *testing.T) {
	goodb, err := NewDB("state.db")
	if err != nil {
		t.Errorf("Unable to create database: %+v", err)
	}
	s := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}},
	}
	err = goodb.WriteStateToDB(s)
	if err != nil {
		t.Errorf("Unable to write packages to db: %v", err)
	}
	pkgs, err := goodb.FetchPkgs()
	if err != nil {
		t.Errorf("Unable to fetch packages: %v", err)
	}
	if !cmp.Equal(s, pkgs, cmpopts.IgnoreFields(client.PackageState{}, "InstallDate")) {
		t.Errorf("GetPackageState did not return expected result, want: %#v, got: %#v", pkgs, s)
	}
	os.Remove("state.db")
}

func TestRemovePackage(t *testing.T) {
	goodb, err := NewDB("state.db")
	if err != nil {
		t.Errorf("Unable to create database: %+v", err)
	}
	s := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}},
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}},
	}
	goodb.WriteStateToDB(s)
	r := client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test1"}},
	}
	goodb.RemovePkg("test2", "")
	// Marshal to json to avoid legacy issues in null fields in nested structs.
	pkgs, err := goodb.FetchPkgs()
	if diff := cmp.Diff(r, pkgs, cmpopts.IgnoreFields(client.PackageState{}, "InstallDate")); diff != "" {
		fmt.Println(diff)
		t.Errorf("GetPackageState did not return expected result, want: %#v, got: %#v", pkgs, s)
	}
	os.Remove("state.db")
}

func compareState(got, want client.GooGetState) {

}
