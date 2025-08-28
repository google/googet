// Package testutil provides common helper functions for tests.
package testutil

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		f, err := os.Open(filepath.Join(dir, r.URL.Path))
		if err != nil {
			t.Logf("couldn't find file: %v", r.URL.Path)
			http.Error(w, "couldn't find requested file", http.StatusNotFound)
		} else {
			io.Copy(w, f)
			f.Close()
		}
	}))
}
