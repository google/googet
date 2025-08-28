package remove

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
)

// genGoo creates a name.noarch.version.goo package file in directory dir for
// the package with given pkgspec. When installed name.goo writes a file having
// same name as the package to the dst directory. The contents of this file is
// "name.noarch.version". Returns a RepoSpec for the goo package.
func genGoo(t *testing.T, dir, dst string, ps goolib.PkgSpec) goolib.RepoSpec {
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

func TestRemoveOne(t *testing.T) {
	logger.Init("GooGet", true, false, io.Discard)
	for _, tc := range []struct {
		desc      string
		pkgName   string
		state     client.GooGetState
		wantState []string
	}{
		{
			desc:    "simple-remove",
			pkgName: "A",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}},
				{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "2"}},
			},
			wantState: []string{"B.noarch.2"},
		},
		{
			desc:    "not-installed",
			pkgName: "C",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}},
				{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "2"}},
			},
			wantState: []string{"A.noarch.1", "B.noarch.2"},
		},
		{
			desc:    "has-dependent-packages",
			pkgName: "A",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "10", PkgDependencies: map[string]string{"D": "4"}}},
				{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "2", PkgDependencies: map[string]string{"A": "2"}}},
				{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "3", PkgDependencies: map[string]string{"A": "10"}}},
				{PackageSpec: &goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "4"}},
			},
			wantState: []string{"D.noarch.4"},
		},
		{
			desc:    "has-chain-of-dependent-packages",
			pkgName: "A",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "10"}},
				{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "2", PkgDependencies: map[string]string{"A": "1"}}},
				{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "3", PkgDependencies: map[string]string{"B": "1"}}},
				{PackageSpec: &goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "4", PkgDependencies: map[string]string{"C": "1"}}},
			},
			wantState: []string{},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			downloader, err := client.NewDownloader("")
			if err != nil {
				t.Fatalf("NewDownloader: %v", err)
			}
			// Generate goos for the packages in state and fix up their local path so
			// that the remove code can find them.
			gooDir, logDir := t.TempDir(), t.TempDir()
			for i, ps := range tc.state {
				rs := genGoo(t, gooDir, logDir, *ps.PackageSpec)
				ps.PackageSpec = rs.PackageSpec // fixes Files
				ps.LocalPath = filepath.Join(gooDir, ps.PackageSpec.String()+".goo")
				ps.Checksum = rs.Checksum
				tc.state[i] = ps
			}
			if err := db.WriteStateToDB(tc.state); err != nil {
				t.Fatalf("db.WriteStateToDB: %v", err)
			}
			// Remove a package.
			cmd := removeCmd{}
			if err := cmd.removeOne(t.Context(), tc.pkgName, downloader, db); err != nil {
				t.Fatalf("removeOne: %v", err)
			}
			// Check that database looks right.
			state, err := db.FetchPkgs("")
			if err != nil {
				t.Fatalf("db.FetchPkgs: %v", err)
			}
			var gotState []string
			for _, ps := range state {
				gotState = append(gotState, ps.PackageSpec.String())
			}
			if diff := cmp.Diff(tc.wantState, gotState, cmpopts.EquateEmpty(), cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Fatalf("unexpected db state (-want +got):\n%v", diff)
			}
		})
	}
}
