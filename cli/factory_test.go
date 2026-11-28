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
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path"
	"strconv"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
)

func TestOpenDBUnprivilegedMissing(t *testing.T) {
	root := t.TempDir()
	settings.Initialize(root, false)
	db, err := OpenDB(context.Background())
	if err == nil {
		db.Close()
		t.Fatal("OpenDB(unprivileged) with no database = nil error, want one")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenDB(unprivileged) with no database = %v, want an error wrapping os.ErrNotExist", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("unprivileged OpenDB left %v in the root (error %v), want nothing", entries, err)
	}
}

func TestOpenDBPrivilegedCreates(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	db, err := OpenDB(WithPrivileged(context.Background()))
	if err != nil {
		t.Fatalf("OpenDB(privileged): %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"}}}); err != nil {
		t.Errorf("writing through the privileged database: %v", err)
	}
	if _, err := os.Stat(settings.DBFile()); err != nil {
		t.Errorf("privileged OpenDB didn't create the database: %v", err)
	}
}

func TestOpenDBUnprivilegedIsReadOnly(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	w, err := OpenDB(WithPrivileged(context.Background()))
	if err != nil {
		t.Fatalf("OpenDB(privileged): %v", err)
	}
	if err := w.WriteStateToDB(client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(context.Background())
	if err != nil {
		t.Fatalf("OpenDB(unprivileged): %v", err)
	}
	if got, err := db.FetchPkgs("foo"); err != nil || len(got) != 1 {
		t.Errorf("FetchPkgs(foo) through the unprivileged database = %v, %v; want one package", got, err)
	}
	if err := db.RemovePkg("foo", "noarch"); err == nil {
		t.Error("RemovePkg through the unprivileged database = nil, want a write error")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("database bytes changed through the unprivileged database")
	}
}

func TestNewDownloaderFactory(t *testing.T) {
	for _, privileged := range []bool{false, true} {
		ctx := context.Background()
		if privileged {
			ctx = WithPrivileged(ctx)
		}
		d, err := NewDownloader(ctx, "")
		if err != nil {
			t.Fatalf("NewDownloader(privileged=%v): %v", privileged, err)
		}
		if d.ReadOnlyCache == privileged {
			t.Errorf("NewDownloader(privileged=%v).ReadOnlyCache = %v, want %v", privileged, d.ReadOnlyCache, !privileged)
		}
	}
}

const (
	cliImport      = "github.com/google/googet/v2/cli"
	clientImport   = "github.com/google/googet/v2/client"
	googetdbImport = "github.com/google/googet/v2/googetdb"

	// factoryFile is the only file in this package and its subpackages that
	// may construct the database and Downloader directly.
	factoryFile = "factory.go"
)

// bannedConstructors lists, by import path, the functions that only
// factoryFile may refer to.
var bannedConstructors = map[string]map[string]bool{
	googetdbImport: {"NewDB": true, "OpenReadOnly": true, "CreateIfMissing": true},
	clientImport:   {"NewDownloader": true},
}

// factories lists the functions in factoryFile, which subpackages may call
// only from a command's Execute method.
var factories = map[string]bool{"OpenDB": true, "NewDownloader": true}

// selectorTarget returns the import path and name that e refers to when e is
// a qualified identifier such as googetdb.NewDB, and ok false otherwise.
func (gf guardFile) selectorTarget(e ast.Expr) (importPath, name string, ok bool) {
	sel, isSel := e.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	x, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return "", "", false
	}
	p, isImport := gf.imports[x.Name]
	if !isImport {
		return "", "", false
	}
	return p, sel.Sel.Name, true
}

// factoryViolations returns the uses in gf that TestFactoriesOnly forbids,
// and the number of factory calls it found in a subpackage. Outside
// factoryFile, gf may not:
//   - dot-import googetdb or client;
//   - refer to a function in bannedConstructors;
//   - use client.Downloader other than as *client.Downloader, so it can't
//     declare, allocate or build a Downloader value;
//   - refer to a ReadOnlyCache field;
//   - call a factory outside a command's Execute method, in a subpackage.
func factoryViolations(gf guardFile, fset *token.FileSet) (violations []string, factoryCalls int) {
	report := func(n ast.Node, format string, args ...any) {
		violations = append(violations, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
	}
	for _, imp := range gf.file.Imports {
		if imp.Name == nil || imp.Name.Name != "." {
			continue
		}
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && bannedConstructors[p] != nil {
			report(imp, "dot import of %q; refer to it only from %s", p, factoryFile)
		}
	}
	if gf.name == factoryFile {
		return violations, 0
	}
	inSubpackage := gf.dir() != "."
	// Selectors that are the operand of a pointer type, as in
	// *client.Downloader.
	pointed := map[ast.Expr]bool{}
	ast.Inspect(gf.file, func(n ast.Node) bool {
		if star, ok := n.(*ast.StarExpr); ok {
			pointed[star.X] = true
		}
		return true
	})
	for _, decl := range gf.file.Decls {
		fd, isFunc := decl.(*ast.FuncDecl)
		inExecute := isFunc && fd.Recv != nil && fd.Name.Name == "Execute"
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "ReadOnlyCache" {
				report(sel, "ReadOnlyCache used; only cli.NewDownloader may set it")
			}
			p, name, ok := gf.selectorTarget(sel)
			if !ok {
				return true
			}
			if bannedConstructors[p][name] {
				report(sel, "%s.%s used directly; use cli.OpenDB or cli.NewDownloader", path.Base(p), name)
			}
			if p == clientImport && name == "Downloader" && !pointed[sel] {
				report(sel, "client.Downloader used as a value; use the *client.Downloader from cli.NewDownloader, which doesn't write the cache in an unelevated run")
			}
			if inSubpackage && p == cliImport && factories[name] {
				factoryCalls++
				if !inExecute {
					report(sel, "cli.%s called outside a command's Execute method; call it in Execute and pass the result on", name)
				}
			}
			return true
		})
	}
	return violations, factoryCalls
}

// TestFactoriesOnly checks that commands get the googet database and a
// Downloader only through OpenDB and NewDownloader, called from Execute.
// cli.RefuseUnlessAllowed and cli.RefuseUnlessPrivileged are what stop a
// command from doing privileged work in an unelevated run; this guard only
// makes sure that such a run can't open the database read-write or write the
// repo index cache.
func TestFactoriesOnly(t *testing.T) {
	files, fset := parseGuardFiles(t)
	var subpackageFiles, factoryCalls int
	for _, gf := range files {
		if gf.dir() != "." {
			subpackageFiles++
		}
		violations, calls := factoryViolations(gf, fset)
		factoryCalls += calls
		for _, v := range violations {
			t.Error(v)
		}
	}
	// Guard against scanning the wrong files and passing without checking
	// anything.
	if subpackageFiles == 0 || factoryCalls == 0 {
		t.Fatalf("scanned %d subpackage files with %d factory calls; want both nonzero", subpackageFiles, factoryCalls)
	}
}

// TestFactoryViolations checks factoryViolations on small sources, so that
// TestFactoriesOnly can't pass by missing a forbidden use.
func TestFactoryViolations(t *testing.T) {
	const header = `package x

import (
	"context"

	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
)

var (
	_ = context.Background
	_ = cli.IsPrivileged
	_ *client.Downloader
	_ = googetdb.Exists
)

type cmd struct{}
`
	for _, tc := range []struct {
		desc      string
		name      string // File name; defaults to a subpackage file.
		body      string
		wantCount int // Number of violations, not counting the header.
		wantCalls int
	}{
		{desc: "pointer parameter", body: "func f(d *client.Downloader) {}"},
		{desc: "factories in Execute", body: "func (c *cmd) Execute(ctx context.Context) { cli.OpenDB(ctx); cli.NewDownloader(ctx, \"\") }", wantCalls: 2},
		{desc: "factory outside Execute", body: "func f(ctx context.Context) { cli.OpenDB(ctx) }", wantCount: 1, wantCalls: 1},
		{desc: "factory in package cli", name: "x.go", body: "func f(ctx context.Context) { cli.OpenDB(ctx) }"},
		{desc: "NewDB", body: "func f() { googetdb.NewDB(\"x\") }", wantCount: 1},
		{desc: "OpenReadOnly", body: "func f() { googetdb.OpenReadOnly(\"x\") }", wantCount: 1},
		{desc: "CreateIfMissing", body: "func f() { googetdb.CreateIfMissing(\"x\") }", wantCount: 1},
		{desc: "client.NewDownloader", body: "func f() { client.NewDownloader(\"\") }", wantCount: 1},
		{desc: "Downloader variable", body: "func f() { var d client.Downloader; _ = d }", wantCount: 1},
		{desc: "Downloader literal", body: "func f() { _ = &client.Downloader{} }", wantCount: 1},
		{desc: "new Downloader", body: "func f() { _ = new(client.Downloader) }", wantCount: 1},
		{desc: "Downloader field", body: "type s struct{ d client.Downloader }", wantCount: 1},
		{desc: "ReadOnlyCache assignment", body: "func f(d *client.Downloader) { d.ReadOnlyCache = false }", wantCount: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			name := tc.name
			if name == "" {
				name = "x/x.go"
			}
			fset := token.NewFileSet()
			gf, err := newGuardFile(fset, name, header+tc.body+"\n")
			if err != nil {
				t.Fatal(err)
			}
			violations, calls := factoryViolations(gf, fset)
			if len(violations) != tc.wantCount || calls != tc.wantCalls {
				t.Errorf("factoryViolations(%q) = %q, %d calls; want %d violations and %d calls", tc.body, violations, calls, tc.wantCount, tc.wantCalls)
			}
		})
	}
	t.Run("dot import", func(t *testing.T) {
		fset := token.NewFileSet()
		gf, err := newGuardFile(fset, "x/x.go", "package x\n\nimport . \"github.com/google/googet/v2/googetdb\"\n\nvar _ = Exists\n")
		if err != nil {
			t.Fatal(err)
		}
		if violations, _ := factoryViolations(gf, fset); len(violations) != 1 {
			t.Errorf("factoryViolations(dot import of googetdb) = %q, want one violation", violations)
		}
	})
}
