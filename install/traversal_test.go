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

package install

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
)

func TestInstallPkgFileSourceOutsideDir(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// The package extracts to <root>/pkg. A spec from a repo index or the
	// database can name a source outside it, such as <root>/secret.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("not part of the package"), 0644); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "pkg.goo")
	f, err := os.Create(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "inside", Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	ps := goolib.PkgSpec{Files: map[string]string{"../secret": filepath.Join(dst, "copied")}}
	if _, err := installPkg(pkg, &ps, false, false, db); err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("installPkg error = %v, want a path traversal error", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "copied")); !os.IsNotExist(err) {
		t.Errorf("installPkg copied a file from outside the package: stat error = %v", err)
	}
}
