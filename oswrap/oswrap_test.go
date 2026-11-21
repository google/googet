/*
Copyright 2016 Google Inc. All Rights Reserved.
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

package oswrap

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestRootDir(t *testing.T) {
	var table = []struct {
		path, want string
	}{
		{"/linux/abs/path", "/linux"},
		{"linux/rel/path", "linux"},
		{"/path", "/path"},
	}

	if runtime.GOOS == "windows" {
		table = append(table, []struct{ path, want string }{
			{`Z:\windows\abs\path`, `Z:\windows`},
			{`\windows\rel\path`, `\windows`},
		}...)
	}

	for _, tt := range table {
		if got := rootDir(tt.path); got != tt.want {
			t.Fatalf("rootDir did not return expected path, got: %q, want: %q ", got, tt.want)
		}
	}
}

func TestOpenSharedRoundTrip(t *testing.T) {
	name := filepath.Join(t.TempDir(), "file")
	want := []byte("shared read content")
	if err := os.WriteFile(name, want, 0644); err != nil {
		t.Fatalf("writing %q: %v", name, err)
	}

	f, err := OpenShared(name)
	if err != nil {
		t.Fatalf("OpenShared(%q): %v", name, err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Errorf("reading %q: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("closing %q: %v", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("OpenShared(%q) read %q, want %q", name, got, want)
	}
	if f.Name() != name {
		t.Errorf("OpenShared(%q).Name() = %q, want %q", name, f.Name(), name)
	}

	got, err = ReadFileShared(name)
	if err != nil {
		t.Fatalf("ReadFileShared(%q): %v", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ReadFileShared(%q) = %q, want %q", name, got, want)
	}
}

func TestOpenSharedMissing(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		filepath.Join(dir, "missing"),
		filepath.Join(dir, "missing-dir", "file"),
	} {
		_, openErr := OpenShared(name)
		_, readErr := ReadFileShared(name)
		for fn, err := range map[string]error{"OpenShared": openErr, "ReadFileShared": readErr} {
			if !os.IsNotExist(err) || !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s(%q) = %v, want an error matching os.IsNotExist and fs.ErrNotExist", fn, name, err)
			}
			var pe *fs.PathError
			if !errors.As(err, &pe) {
				t.Errorf("%s(%q) = %T, want a *fs.PathError", fn, name, err)
			} else if pe.Path != name {
				t.Errorf("%s(%q) PathError.Path = %q, want the name as passed", fn, name, pe.Path)
			}
		}
	}
}

func TestReadFileSharedDirectory(t *testing.T) {
	dir := t.TempDir()
	if b, err := ReadFileShared(dir); err == nil {
		t.Errorf("ReadFileShared(directory) = %q, nil; want an error", b)
	}
}

// sharedReadDirs lists the packages, relative to this one, whose reads can run
// without the googet lock. Their non-test files must read files only through
// OpenShared or ReadFileShared. Anyone who adds a package that reads files on a
// lock-free path, for example a command that run() in googet.go executes
// without the lock, must add its directory here.
var sharedReadDirs = []string{"../client", "../repo", "../settings"}

// sharedReadAllowlist lists the raw read opens allowed in sharedReadDirs, keyed
// by "dir/file.go:Func", with the reason for each one.
var sharedReadAllowlist = map[string]string{}

// bannedReads lists, by import path, the functions that open files for
// reading without sharing delete access on Windows.
var bannedReads = map[string]map[string]bool{
	"os":                                 {"Open": true, "ReadFile": true},
	"io/ioutil":                          {"ReadFile": true},
	"github.com/google/googet/v2/oswrap": {"Open": true},
}

// openFileFuncs lists, by import path, the OpenFile functions that are banned
// when called with read-only flags.
var openFileFuncs = map[string]bool{
	"os":                                 true,
	"github.com/google/googet/v2/oswrap": true,
}

// rawRead is a raw read open found in a source file.
type rawRead struct {
	key  string // "dir/file.go:Func"
	pos  token.Position
	call string // for example "os.ReadFile"
}

// isWriteFlag reports whether the OpenFile flag expression asks for write
// access, by naming O_WRONLY or O_RDWR anywhere in it.
func isWriteFlag(e ast.Expr) bool {
	write := false
	ast.Inspect(e, func(n ast.Node) bool {
		var name string
		switch n := n.(type) {
		case *ast.SelectorExpr:
			name = n.Sel.Name
		case *ast.Ident:
			name = n.Name
		}
		if name == "O_WRONLY" || name == "O_RDWR" {
			write = true
		}
		return !write
	})
	return write
}

// funcName returns the name of a function declaration, with its receiver
// type if it has one.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if g, ok := t.(*ast.IndexExpr); ok {
		t = g.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// findRawReads returns the raw read opens in a parsed file. prefix is the
// "dir/file.go" part of each key.
func findRawReads(fset *token.FileSet, f *ast.File, prefix string) []rawRead {
	// Map each local import name to its import path.
	imports := map[string]string{}
	for _, is := range f.Imports {
		p, err := strconv.Unquote(is.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if is.Name != nil {
			name = is.Name.Name
		}
		imports[name] = p
	}
	resolve := func(e ast.Expr) (string, string, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", "", false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Obj != nil {
			// id.Obj is set for local variables, which shadow imports.
			return "", "", false
		}
		p, ok := imports[id.Name]
		return p, sel.Sel.Name, ok
	}

	var found []rawRead
	report := func(n ast.Node, fn string) {
		sel := n.(*ast.SelectorExpr)
		found = append(found, rawRead{
			key:  prefix + ":" + fn,
			pos:  fset.Position(n.Pos()),
			call: sel.X.(*ast.Ident).Name + "." + sel.Sel.Name,
		})
	}
	check := func(body ast.Node, fn string) {
		// OpenFile selectors that are called with write flags.
		writes := map[ast.Expr]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if p, name, ok := resolve(call.Fun); ok && openFileFuncs[p] && name == "OpenFile" {
				if len(call.Args) >= 2 && isWriteFlag(call.Args[1]) {
					writes[call.Fun] = true
				}
			}
			return true
		})
		// Any other reference to a banned function is reported, including
		// function values, whose flags can't be checked.
		ast.Inspect(body, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			p, name, ok := resolve(e)
			if !ok {
				return true
			}
			if bannedReads[p][name] || (openFileFuncs[p] && name == "OpenFile" && !writes[e]) {
				report(e, fn)
			}
			return true
		})
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			if fd.Body != nil {
				check(fd.Body, funcName(fd))
			}
			continue
		}
		check(d, "<package>")
	}
	return found
}

func TestNoRawReadOpens(t *testing.T) {
	for _, dir := range sharedReadDirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		parsed := 0
		for _, path := range matches {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			parsed++
			prefix := filepath.Base(dir) + "/" + filepath.Base(path)
			for _, r := range findRawReads(fset, f, prefix) {
				if _, ok := sharedReadAllowlist[r.key]; ok {
					continue
				}
				t.Errorf("%s: %s in %s opens a file for reading without sharing delete access; use oswrap.ReadFileShared or oswrap.OpenShared, or add %q to sharedReadAllowlist with a reason", r.pos, r.call, r.key, r.key)
			}
		}
		if parsed == 0 {
			t.Errorf("found no non-test Go files in %s", dir)
		}
	}
}

// TestFindRawReads checks that the guard above detects what it bans and
// doesn't flag writes.
func TestFindRawReads(t *testing.T) {
	const src = `package p

import (
	"io/ioutil"
	"os"
	gg "github.com/google/googet/v2/oswrap"
)

var opener = os.Open

func reads() {
	os.Open("a")
	os.ReadFile("a")
	ioutil.ReadFile("a")
	gg.Open("a")
	os.OpenFile("a", os.O_RDONLY, 0)
	gg.OpenFile("a", 0, 0)
}

func writes() {
	os.OpenFile("a", os.O_WRONLY|os.O_CREATE, 0)
	gg.OpenFile("a", os.O_RDWR, 0)
	os.Create("a")
	os.WriteFile("a", nil, 0)
	gg.ReadFileShared("a")
	os := struct{ Open func() }{}
	os.Open()
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range findRawReads(fset, f, "p/p.go") {
		got = append(got, r.key+" "+r.call)
	}
	want := []string{
		"p/p.go:<package> os.Open",
		"p/p.go:reads os.Open",
		"p/p.go:reads os.ReadFile",
		"p/p.go:reads ioutil.ReadFile",
		"p/p.go:reads gg.Open",
		"p/p.go:reads os.OpenFile",
		"p/p.go:reads gg.OpenFile",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findRawReads() =\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
