// Package testutil provides common helper functions for tests.
package testutil

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/googet/v2/goolib"
)

// GenGoo creates a name.noarch.version.goo package file in directory dir for
// the package with given pkgspec. When installed name.goo writes a file having
// same name as the package to the dst directory. The contents of this file is
// "name.noarch.version". Returns a RepoSpec for the goo package.
func GenGoo(t *testing.T, dir, dst string, ps goolib.PkgSpec) goolib.RepoSpec {
	t.Helper()
	ps.Files = map[string]string{ps.Name: filepath.Join(dst, ps.Name)}
	b, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, ps.String()+".goo"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	gw := gzip.NewWriter(io.MultiWriter(h, f))
	tw := tar.NewWriter(gw)
	modTime := time.Now()
	for _, x := range []struct {
		name    string
		content []byte
	}{
		{ps.Name, []byte(ps.String())},
		{ps.Name + ".pkgspec", b},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: x.name, Mode: 0644, Size: int64(len(x.content)), ModTime: modTime}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(x.content); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gw.Close()
	return goolib.RepoSpec{
		Checksum:    fmt.Sprintf("%x", h.Sum(nil)),
		Source:      filepath.Base(f.Name()),
		PackageSpec: &ps,
	}
}

// ServeGoo returns an HTTP server that serves files from dir.
func ServeGoo(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			t.Fatal(err)
		}
		absPath, err := filepath.Abs(filepath.Join(absDir, r.URL.Path))
		if err != nil || !strings.HasPrefix(absPath, absDir) {
			http.Error(w, "Invalid file name", http.StatusBadRequest)
			return
		}
		f, err := os.Open(absPath)
		if err != nil {
			t.Logf("couldn't find file: %v", r.URL.Path)
			http.Error(w, "couldn't find requested file", http.StatusNotFound)
		} else {
			io.Copy(w, f)
			f.Close()
		}
	}))
}

// TreeSnapshot records every path under root, including root itself as ".",
// with its mode, size, modification time and, for a regular file, the SHA-256
// of its contents. It returns nil if root doesn't exist, so comparing two
// snapshots also shows whether root was created or removed.
//
// Each path's metadata comes from os.Lstat, not from the directory listing.
// On Windows a listing reports the copy of an entry's metadata that NTFS
// keeps in the parent directory, which can lag the entry itself: after a
// file is created in a directory, the parent can keep listing the
// directory's older modification time until something, such as this walk,
// opens the directory. Two listing-based snapshots of an unchanged tree
// could then differ.
func TreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	}
	s := map[string]string{}
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		v := fmt.Sprintf("%v %d %d", fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += fmt.Sprintf(" %x", sha256.Sum256(b))
		}
		s[filepath.ToSlash(rel)] = v
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", root, err)
	}
	return s
}
