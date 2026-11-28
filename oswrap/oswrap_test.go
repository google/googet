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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
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

// tempLeftovers returns the names in dir that look like WriteFileAtomic
// temporary files.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %q: %v", dir, err)
	}
	var left []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			left = append(left, e.Name())
		}
	}
	return left
}

func TestWriteFileAtomicReplaces(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "file")
	if err := os.WriteFile(name, []byte("old contents"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	// On Unix, a handle opened before the write keeps reading the old file.
	// (On Windows, os.Open doesn't share delete access, so it would block
	// the rename.)
	var old *os.File
	if runtime.GOOS != "windows" {
		if old, err = os.Open(name); err != nil {
			t.Fatal(err)
		}
		defer old.Close()
	}

	if err := WriteFileAtomic(name, []byte("new"), 0644); err != nil {
		t.Fatalf("WriteFileAtomic(%q): %v", name, err)
	}
	if got, err := os.ReadFile(name); err != nil || string(got) != "new" {
		t.Errorf("after WriteFileAtomic, %q holds %q (error %v), want %q", name, got, err, "new")
	}
	after, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if os.SameFile(before, after) {
			t.Error("WriteFileAtomic rewrote the file in place, want a new file renamed over it")
		}
		if got, err := io.ReadAll(old); err != nil || string(got) != "old contents" {
			t.Errorf("handle opened before WriteFileAtomic read %q (error %v), want the old contents", got, err)
		}
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Errorf("WriteFileAtomic left temporary files %q", left)
	}
}

func TestWriteFileAtomicPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		desc     string
		existing os.FileMode // 0 means no existing file.
		perm     os.FileMode
	}{
		{desc: "new file", perm: 0600},
		{desc: "new file 0644", perm: 0644},
		{desc: "replaced file gets perm", existing: 0600, perm: 0640},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			name := filepath.Join(dir, strings.ReplaceAll(tc.desc, " ", "_"))
			if tc.existing != 0 {
				if err := os.WriteFile(name, []byte("x"), tc.existing); err != nil {
					t.Fatal(err)
				}
			}
			if err := WriteFileAtomic(name, []byte("data"), tc.perm); err != nil {
				t.Fatalf("WriteFileAtomic: %v", err)
			}
			fi, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tc.perm {
				t.Errorf("mode = %v, want %v, whatever the umask", got, tc.perm)
			}
		})
	}
}

func TestWriteFileAtomicErrors(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "missing", "file")
		if err := WriteFileAtomic(name, []byte("data"), 0644); err == nil {
			t.Errorf("WriteFileAtomic(%q) = nil, want an error", name)
		}
		if _, err := os.Lstat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
			t.Errorf("WriteFileAtomic created the missing directory (Lstat error: %v)", err)
		}
	})
	t.Run("target is a directory", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "target")
		if err := os.Mkdir(name, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(name, "inside"), []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := WriteFileAtomic(name, []byte("data"), 0644); err == nil {
			t.Errorf("WriteFileAtomic over a directory = nil, want an error")
		}
		if got, err := os.ReadFile(filepath.Join(name, "inside")); err != nil || string(got) != "keep" {
			t.Errorf("directory contents after the failed write = %q (error %v), want them intact", got, err)
		}
		if left := tempLeftovers(t, dir); len(left) != 0 {
			t.Errorf("failed WriteFileAtomic left temporary files %q", left)
		}
	})
	t.Run("unwritable directory", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs Unix permissions and a non-root user")
		}
		dir := t.TempDir()
		name := filepath.Join(dir, "file")
		if err := os.WriteFile(name, []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0755)
		if err := WriteFileAtomic(name, []byte("new"), 0644); err == nil {
			t.Errorf("WriteFileAtomic in a read-only directory = nil, want an error")
		}
		if got, err := os.ReadFile(name); err != nil || string(got) != "old" {
			t.Errorf("after the failed write %q holds %q (error %v), want %q", name, got, err, "old")
		}
		if left := tempLeftovers(t, dir); len(left) != 0 {
			t.Errorf("failed WriteFileAtomic left temporary files %q", left)
		}
	})
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

// listDir returns the names in dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %q: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRenameAsideHelper(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		got, err := renameAside(filepath.Join(t.TempDir(), "missing"))
		if got != "" || err != nil {
			t.Errorf("renameAside(missing) = %q, %v; want \"\", nil", got, err)
		}
	})
	for _, base := range []string{
		"googet.repo",
		strings.Repeat("a", 200) + ".rs",
		"a" + strings.Repeat("é", 40) + ".repo", // The cut at maxAsideBase falls inside "é".
		strings.Repeat("日本", 30),                // The cut falls inside a 3-byte character.
	} {
		t.Run(fmt.Sprintf("%d-byte base", len(base)), func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, base)
			if err := os.WriteFile(name, []byte("contents"), 0644); err != nil {
				t.Fatal(err)
			}
			tmp, err := renameAside(name)
			if err != nil {
				t.Fatalf("renameAside(%q): %v", base, err)
			}
			if filepath.Dir(tmp) != dir {
				t.Errorf("renameAside(%q) = %q, want a name in %q", base, tmp, dir)
			}
			tb := filepath.Base(tmp)
			if !strings.HasPrefix(tb, ".") || !strings.Contains(tb, ".del-") {
				t.Errorf("renameAside(%q) base = %q, want a dot-prefixed name containing .del-", base, tb)
			}
			if !utf8.ValidString(tb) {
				t.Errorf("renameAside(%q) base = %q, want valid UTF-8", base, tb)
			}
			if kept := strings.TrimPrefix(tb[:strings.LastIndex(tb, ".del-")], "."); len(kept) > maxAsideBase || !strings.HasPrefix(base, kept) {
				t.Errorf("renameAside(%q) kept %q of the base, want a prefix of at most %d bytes", base, kept, maxAsideBase)
			}
			if _, err := os.Lstat(name); !os.IsNotExist(err) {
				t.Errorf("%q still exists after renameAside (Lstat error: %v)", name, err)
			}
			if b, err := os.ReadFile(tmp); err != nil || string(b) != "contents" {
				t.Errorf("renamed file holds %q (error %v), want the original contents", b, err)
			}
		})
	}
	t.Run("unique", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "f")
		seen := map[string]bool{}
		for i := 0; i < 5; i++ {
			if err := os.WriteFile(name, nil, 0644); err != nil {
				t.Fatal(err)
			}
			tmp, err := renameAside(name)
			if err != nil {
				t.Fatal(err)
			}
			if seen[tmp] {
				t.Errorf("renameAside returned %q twice", tmp)
			}
			seen[tmp] = true
		}
	})
	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need extra privileges on Windows")
		}
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("t"), 0644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		tmp, err := renameAside(link)
		if err != nil {
			t.Fatalf("renameAside(symlink): %v", err)
		}
		if fi, err := os.Lstat(tmp); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("renameAside(symlink) moved %v (error %v), want the link itself", fi, err)
		}
		if b, err := os.ReadFile(target); err != nil || string(b) != "t" {
			t.Errorf("symlink target holds %q (error %v), want it untouched", b, err)
		}
	})
}

// removeCases are the cases that TestRenameAndRemove and TestRemoveOrPark
// share. setup creates the path to remove in dir and returns it. want lists
// the paths, relative to dir, that must remain after the removal.
var removeCases = []struct {
	desc    string
	setup   func(t *testing.T, dir string) string
	wantErr bool
	want    []string
}{
	{
		desc:  "missing",
		setup: func(t *testing.T, dir string) string { return filepath.Join(dir, "missing") },
	},
	{
		desc: "file",
		setup: func(t *testing.T, dir string) string {
			name := filepath.Join(dir, "x.rs")
			if err := os.WriteFile(name, []byte("x"), 0644); err != nil {
				t.Fatal(err)
			}
			return name
		},
	},
	{
		desc: "empty directory",
		setup: func(t *testing.T, dir string) string {
			name := filepath.Join(dir, "d")
			if err := os.Mkdir(name, 0755); err != nil {
				t.Fatal(err)
			}
			return name
		},
	},
	{
		desc: "non-empty directory",
		setup: func(t *testing.T, dir string) string {
			name := filepath.Join(dir, "d")
			if err := os.Mkdir(name, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(name, "keep"), []byte("keep"), 0644); err != nil {
				t.Fatal(err)
			}
			return name
		},
		wantErr: true,
		want:    []string{"d", filepath.Join("d", "keep")},
	},
}

// listTree returns the paths of everything below dir, relative to dir.
func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			names = append(names, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", dir, err)
	}
	return names
}

func TestRenameAndRemove(t *testing.T) {
	for _, tc := range removeCases {
		t.Run(tc.desc, func(t *testing.T) {
			dir := t.TempDir()
			name := tc.setup(t, dir)
			if err := RenameAndRemove(name); (err != nil) != tc.wantErr {
				t.Errorf("RenameAndRemove(%s) = %v, want error: %t", tc.desc, err, tc.wantErr)
			}
			if got := listTree(t, dir); !slices.Equal(got, tc.want) {
				t.Errorf("after RenameAndRemove(%s), directory holds %q, want %q", tc.desc, got, tc.want)
			}
		})
	}
	t.Run("held open", func(t *testing.T) {
		// On Windows this exercises the delete-pending case: the path must be
		// free for a new file at once.
		dir := t.TempDir()
		name := filepath.Join(dir, "x.rs")
		if err := os.WriteFile(name, []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		f, err := OpenShared(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := RenameAndRemove(name); err != nil {
			f.Close()
			t.Fatalf("RenameAndRemove of an open file: %v", err)
		}
		if err := os.WriteFile(name, []byte("new"), 0644); err != nil {
			t.Errorf("creating %q again while the old file is open: %v", name, err)
		}
		if b, err := io.ReadAll(f); err != nil || string(b) != "old" {
			t.Errorf("open handle read %q (error %v), want the old contents", b, err)
		}
		f.Close()
		if got := listDir(t, dir); len(got) != 1 || got[0] != "x.rs" {
			t.Errorf("directory holds %q after the handle closed, want only x.rs", got)
		}
	})
}

func TestRemoveOrPark(t *testing.T) {
	for _, tc := range removeCases {
		t.Run(tc.desc, func(t *testing.T) {
			dir := t.TempDir()
			parkDir := t.TempDir()
			name := tc.setup(t, dir)
			left, err := RemoveOrPark(name, parkDir)
			if left != "" || (err != nil) != tc.wantErr {
				t.Errorf("RemoveOrPark(%s) = %q, %v; want \"\", error: %t", tc.desc, left, err, tc.wantErr)
			}
			if got := listTree(t, dir); !slices.Equal(got, tc.want) {
				t.Errorf("after RemoveOrPark(%s), directory holds %q, want %q", tc.desc, got, tc.want)
			}
			if got := listDir(t, parkDir); len(got) != 0 {
				t.Errorf("after RemoveOrPark(%s), park directory holds %q, want nothing", tc.desc, got)
			}
		})
	}
}

// TestRemoveOrParkUndeletable checks that RemoveOrPark moves a file whose
// delete fails into parkDir and returns the file's new name.
func TestRemoveOrParkUndeletable(t *testing.T) {
	errDelete := errors.New("delete failed")
	orig := removeAside
	t.Cleanup(func() { removeAside = orig })
	removeAside = func(string) error { return errDelete }

	dir := t.TempDir()
	parkDir := t.TempDir()
	name := filepath.Join(dir, "pkg.dll")
	if err := os.WriteFile(name, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	left, err := RemoveOrPark(name, parkDir)
	if filepath.Dir(left) != parkDir || !strings.HasPrefix(filepath.Base(left), "pkg.dll.old") {
		t.Fatalf("RemoveOrPark() = %q, %v; want a name starting with pkg.dll.old in %q", left, err, parkDir)
	}
	if b, rerr := os.ReadFile(left); rerr != nil || string(b) != "old" {
		t.Errorf("parked file holds %q (error %v), want %q", b, rerr, "old")
	}
	if got := listDir(t, dir); len(got) != 0 {
		t.Errorf("after RemoveOrPark, directory holds %q, want nothing", got)
	}
	// Only Windows can schedule removal on reboot; elsewhere the left-behind
	// file is reported as an error.
	if runtime.GOOS != "windows" {
		if err == nil || !strings.Contains(err.Error(), left) || !strings.Contains(err.Error(), errDelete.Error()) {
			t.Errorf("RemoveOrPark() error = %v, want an error naming %q and including %q", err, left, errDelete)
		}
	}
}

func TestRetryWhile(t *testing.T) {
	errTransient := errors.New("transient")
	errOther := errors.New("other")
	transient := func(err error) bool { return errors.Is(err, errTransient) }
	for _, tc := range []struct {
		desc      string
		errs      []error // op returns these in turn, then nil.
		want      error
		wantCalls int
	}{
		{desc: "success", wantCalls: 1},
		{desc: "transient then success", errs: []error{errTransient, errTransient}, wantCalls: 3},
		{desc: "other error", errs: []error{errOther}, want: errOther, wantCalls: 1},
		{desc: "transient then other", errs: []error{errTransient, errOther}, want: errOther, wantCalls: 2},
		{desc: "always transient", errs: slices.Repeat([]error{errTransient}, replaceAttempts+1), want: errTransient, wantCalls: replaceAttempts},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			calls := 0
			err := retryWhile(transient, func() error {
				calls++
				if calls <= len(tc.errs) {
					return tc.errs[calls-1]
				}
				return nil
			})
			if err != tc.want || calls != tc.wantCalls {
				t.Errorf("retryWhile() = %v after %d calls, want %v after %d calls", err, calls, tc.want, tc.wantCalls)
			}
		})
	}
}

func TestPark(t *testing.T) {
	src := t.TempDir()
	parkDir := t.TempDir()
	name := filepath.Join(src, ".pkg.dll.del-0123")
	if err := os.WriteFile(name, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	parked, err := park(name, "pkg.dll", parkDir)
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	if filepath.Dir(parked) != parkDir || !strings.HasPrefix(filepath.Base(parked), "pkg.dll.old") {
		t.Errorf("park() = %q, want a name starting with pkg.dll.old in %q", parked, parkDir)
	}
	if b, err := os.ReadFile(parked); err != nil || string(b) != "old" {
		t.Errorf("parked file holds %q (error %v), want %q", b, err, "old")
	}
	if left := listDir(t, src); len(left) != 0 {
		t.Errorf("source directory after park holds %q, want nothing", left)
	}
	if got := listDir(t, parkDir); len(got) != 1 {
		t.Errorf("park directory holds %q, want only the parked file", got)
	}
}

func TestIsTransientShareErrorUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see TestIsTransientShareErrorWindows")
	}
	for _, err := range []error{nil, os.ErrPermission, fs.ErrNotExist, &fs.PathError{Op: "open", Path: "x", Err: errors.New("busy")}} {
		if IsTransientShareError(err) {
			t.Errorf("IsTransientShareError(%v) = true, want false on %s", err, runtime.GOOS)
		}
	}
}
