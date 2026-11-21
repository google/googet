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

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardFile is a parsed non-test Go file of this package or a subpackage.
type guardFile struct {
	name string // Path relative to this package, such as "install/install.go".
	file *ast.File
}

// dir returns the directory of gf relative to this package, "." for this
// package itself.
func (gf guardFile) dir() string { return filepath.Dir(gf.name) }

// newGuardFile parses the Go file name, from src if it isn't nil.
func newGuardFile(fset *token.FileSet, name string, src any) (guardFile, error) {
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return guardFile{}, err
	}
	return guardFile{name: filepath.ToSlash(name), file: f}, nil
}

// parseGuardFiles parses the non-test Go files of this package and of every
// direct subpackage. The guard tests share it, so they all check the same
// files.
func parseGuardFiles(t *testing.T) (files []guardFile, fset *token.FileSet) {
	t.Helper()
	fset = token.NewFileSet()
	dirs := []string{"."}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	for _, dir := range dirs {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			gf, err := newGuardFile(fset, name, nil)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, gf)
		}
	}
	return files, fset
}
