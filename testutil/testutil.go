// Package testutil provides common helper functions for tests.
package testutil

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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
	return GenGooCompressed(t, dir, dst, ps, goolib.CompressionGzip)
}

// GenGooCompressed is like GenGoo, but compresses the package with format,
// which must be one of goolib.CompressionFormats().
func GenGooCompressed(t *testing.T, dir, dst string, ps goolib.PkgSpec, format string) goolib.RepoSpec {
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
	modTime := time.Now()
	WritePackage(t, io.MultiWriter(h, f), format, []File{
		{Name: ps.Name, Mode: 0644, Body: []byte(ps.String()), ModTime: modTime},
		{Name: ps.Name + ".pkgspec", Mode: 0644, Body: b, ModTime: modTime},
	})
	return goolib.RepoSpec{
		Checksum:    fmt.Sprintf("%x", h.Sum(nil)),
		Source:      filepath.Base(f.Name()),
		PackageSpec: &ps,
	}
}

// File is a regular file entry in a package archive written by WritePackage.
type File struct {
	Name    string
	Mode    int64
	Body    []byte
	ModTime time.Time
}

// WritePackage writes files to w as a tar archive compressed with format,
// which must be one of goolib.CompressionFormats().
func WritePackage(t *testing.T, w io.Writer, format string, files []File) {
	t.Helper()
	cw, err := goolib.NewPackageWriter(w, format)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(cw)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: f.Mode, Size: int64(len(f.Body)), ModTime: f.ModTime}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.Body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("error closing tar writer: %v", err)
	}
	if err := cw.Close(); err != nil {
		t.Fatalf("error closing %s writer: %v", format, err)
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
