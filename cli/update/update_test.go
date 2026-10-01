package update

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func captureStdout(f func()) string {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestUpdates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state client.GooGetState
		rm    client.RepoMap
		want  []goolib.PackageInfo
	}{
		{
			name: "upgrade to later version",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
				{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
			},
			want: []goolib.PackageInfo{{Name: "foo", Arch: "x86_32", Ver: "2.0"}},
		},
		{
			name: "rollback to earlier version",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
				{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
				"rollback": client.Repo{
					Priority: 1500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			want: []goolib.PackageInfo{{Name: "foo", Arch: "x86_32", Ver: "1.0"}},
		},
		{
			name: "no change if rollback version already installed",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
				"rollback": client.Repo{
					Priority: 1500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			want: nil,
		},
		{
			name: "no updates available",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			settings.Archs = []string{"x86_32", "noarch"}

			var pi []goolib.PackageInfo
			captureStdout(func() {
				pi = updates(tc.state, tc.rm)
			})

			if diff := cmp.Diff(pi, tc.want); diff != "" {
				t.Errorf("updates(%v, %v) got unexpected diff (-got +want):\n%v", tc.state, tc.rm, diff)
			}
		})
	}
}

func TestBatchUpdateContinuation_WhatRepoError(t *testing.T) {
	// Verifies that when client.WhatRepo fails for one package, exitCode = ExitFailure
	// is latched, execution continues, and remaining packages update successfully.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}

	// Initial DB state: pkgA and pkgB at version 1.0.0.
	initialState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}},
	}
	if err := db.WriteStateToDB(initialState); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// Generate update only for pkgB. pkgA update is absent from the repo index.
	pkgB2 := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "2.0.0"}
	rsB2 := testutil.GenGoo(t, gooDir, logDir, pkgB2)

	// In the repo index, provide a virtual provider for pkgA that yields an update
	// in FindRepoLatest but whose real package name differs, inducing WhatRepo error.
	pkgProvider := goolib.PkgSpec{
		Name:     "real_provider",
		Arch:     "noarch",
		Version:  "2.0.0",
		Provides: []string{"pkgA"},
	}
	rsProvider := testutil.GenGoo(t, gooDir, logDir, pkgProvider)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsB2, rsProvider})
	if err != nil {
		t.Fatalf("json.Marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "index"), indexBytes, 0644); err != nil {
		t.Fatalf("writing index: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(indexBytes); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}
	gw.Close()
	if err := os.WriteFile(filepath.Join(gooDir, "index.gz"), gzBuf.Bytes(), 0644); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}

	cmd := &updateCmd{sources: srv.URL}
	exitStatus := cmd.Execute(ctx, nil)

	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB was updated to 2.0.0.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil || psB.PackageSpec.Version != "2.0.0" {
		t.Errorf("pkgB version got %v, want 2.0.0", psB.PackageSpec)
	}

	// Verify pkgA remained at 1.0.0.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec == nil || psA.PackageSpec.Version != "1.0.0" {
		t.Errorf("pkgA version got %v, want 1.0.0", psA.PackageSpec)
	}
}

func TestBatchUpdateContinuation_InstallFailure(t *testing.T) {
	// Verifies that when install.FromRepo fails for one package, exitCode = ExitFailure
	// is latched and remaining packages continue to update.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}

	initialState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}},
	}
	if err := db.WriteStateToDB(initialState); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// Generate pkgA and pkgB updates.
	pkgA2 := goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "2.0.0"}
	rsA2 := testutil.GenGoo(t, gooDir, logDir, pkgA2)

	pkgB2 := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "2.0.0"}
	rsB2 := testutil.GenGoo(t, gooDir, logDir, pkgB2)

	// Corrupt pkgA's file on disk to trigger a checksum/unpack failure in install.FromRepo.
	pkgAPath := filepath.Join(gooDir, rsA2.Source)
	if err := os.WriteFile(pkgAPath, []byte("corrupted file content"), 0644); err != nil {
		t.Fatalf("corrupting pkgA: %v", err)
	}

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsA2, rsB2})
	if err != nil {
		t.Fatalf("json.Marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "index"), indexBytes, 0644); err != nil {
		t.Fatalf("writing index: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(indexBytes); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}
	gw.Close()
	if err := os.WriteFile(filepath.Join(gooDir, "index.gz"), gzBuf.Bytes(), 0644); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}

	cmd := &updateCmd{sources: srv.URL}
	exitStatus := cmd.Execute(ctx, nil)

	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB updated successfully.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil || psB.PackageSpec.Version != "2.0.0" {
		t.Errorf("pkgB version got %v, want 2.0.0", psB.PackageSpec)
	}

	// Verify pkgA was NOT updated (remains 1.0.0).
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec == nil || psA.PackageSpec.Version != "1.0.0" {
		t.Errorf("pkgA version got %v, want 1.0.0", psA.PackageSpec)
	}
}

func TestBatchUpdateContinuation_OrderReversal(t *testing.T) {
	// Verifies batch update continuation when successful package precedes failing package.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}

	// Initial DB state with pkgB first, then pkgA.
	initialState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"}},
	}
	if err := db.WriteStateToDB(initialState); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// pkgB update is valid.
	pkgB2 := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "2.0.0"}
	rsB2 := testutil.GenGoo(t, gooDir, logDir, pkgB2)

	// pkgA update has failing installer execution.
	pkgA2 := goolib.PkgSpec{
		Name:    "pkgA",
		Arch:    "noarch",
		Version: "2.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_binary.exe"},
	}
	rsA2 := testutil.GenGoo(t, gooDir, logDir, pkgA2)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsB2, rsA2})
	if err != nil {
		t.Fatalf("json.Marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "index"), indexBytes, 0644); err != nil {
		t.Fatalf("writing index: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(indexBytes); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}
	gw.Close()
	if err := os.WriteFile(filepath.Join(gooDir, "index.gz"), gzBuf.Bytes(), 0644); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}

	cmd := &updateCmd{sources: srv.URL}
	exitStatus := cmd.Execute(ctx, nil)

	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB was updated to 2.0.0.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil || psB.PackageSpec.Version != "2.0.0" {
		t.Errorf("pkgB version got %v, want 2.0.0", psB.PackageSpec)
	}

	// Verify pkgA remained at 1.0.0.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec == nil || psA.PackageSpec.Version != "1.0.0" {
		t.Errorf("pkgA version got %v, want 1.0.0", psA.PackageSpec)
	}
}

func TestBatchUpdateContinuation_ThreePackages_WhatRepoAndInstallFailure(t *testing.T) {
	// Verifies continuation across 3 packages where pkgA fails WhatRepo, pkgB fails install, and pkgC succeeds.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}

	initialState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgC", Arch: "noarch", Version: "1.0.0"}},
	}
	if err := db.WriteStateToDB(initialState); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// pkgA: virtual provider inducing WhatRepo error.
	pkgProvider := goolib.PkgSpec{
		Name:     "provider_for_a",
		Arch:     "noarch",
		Version:  "2.0.0",
		Provides: []string{"pkgA"},
	}
	rsProvider := testutil.GenGoo(t, gooDir, logDir, pkgProvider)

	// pkgB: corrupted payload inducing install error.
	pkgB2 := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "2.0.0"}
	rsB2 := testutil.GenGoo(t, gooDir, logDir, pkgB2)
	pkgBPath := filepath.Join(gooDir, rsB2.Source)
	if err := os.WriteFile(pkgBPath, []byte("corrupted pkgB content"), 0644); err != nil {
		t.Fatalf("corrupting pkgB: %v", err)
	}

	// pkgC: valid update.
	pkgC2 := goolib.PkgSpec{Name: "pkgC", Arch: "noarch", Version: "2.0.0"}
	rsC2 := testutil.GenGoo(t, gooDir, logDir, pkgC2)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsProvider, rsB2, rsC2})
	if err != nil {
		t.Fatalf("json.Marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "index"), indexBytes, 0644); err != nil {
		t.Fatalf("writing index: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(indexBytes); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}
	gw.Close()
	if err := os.WriteFile(filepath.Join(gooDir, "index.gz"), gzBuf.Bytes(), 0644); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}

	cmd := &updateCmd{sources: srv.URL}
	exitStatus := cmd.Execute(ctx, nil)

	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgC was updated to 2.0.0.
	psC, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgC", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgC): %v", err)
	}
	if psC.PackageSpec == nil || psC.PackageSpec.Version != "2.0.0" {
		t.Errorf("pkgC version got %v, want 2.0.0", psC.PackageSpec)
	}

	// Verify pkgA remained at 1.0.0.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec == nil || psA.PackageSpec.Version != "1.0.0" {
		t.Errorf("pkgA version got %v, want 1.0.0", psA.PackageSpec)
	}

	// Verify pkgB remained at 1.0.0.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil || psB.PackageSpec.Version != "1.0.0" {
		t.Errorf("pkgB version got %v, want 1.0.0", psB.PackageSpec)
	}
}

func TestBatchUpdate_AllSucceed(t *testing.T) {
	// Verifies batch update returns ExitSuccess when all packages update successfully.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}

	initialState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}},
	}
	if err := db.WriteStateToDB(initialState); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	pkgA2 := goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "2.0.0"}
	rsA2 := testutil.GenGoo(t, gooDir, logDir, pkgA2)

	pkgB2 := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "2.0.0"}
	rsB2 := testutil.GenGoo(t, gooDir, logDir, pkgB2)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsA2, rsB2})
	if err != nil {
		t.Fatalf("json.Marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "index"), indexBytes, 0644); err != nil {
		t.Fatalf("writing index: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(indexBytes); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}
	gw.Close()
	if err := os.WriteFile(filepath.Join(gooDir, "index.gz"), gzBuf.Bytes(), 0644); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}

	cmd := &updateCmd{sources: srv.URL}
	exitStatus := cmd.Execute(ctx, nil)

	if exitStatus != subcommands.ExitSuccess {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitSuccess (%v)", exitStatus, subcommands.ExitSuccess)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	for _, name := range []string{"pkgA", "pkgB"} {
		ps, err := db.FetchPkg(goolib.PackageInfo{Name: name, Arch: "noarch"})
		if err != nil {
			t.Fatalf("db.FetchPkg(%s): %v", name, err)
		}
		if ps.PackageSpec == nil || ps.PackageSpec.Version != "2.0.0" {
			t.Errorf("%s version got %v, want 2.0.0", name, ps.PackageSpec)
		}
	}
}

func TestBatchUpdate_AllFail(t *testing.T) {
	// Verifies batch update returns ExitFailure when all package updates fail.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}

	initialState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}},
	}
	if err := db.WriteStateToDB(initialState); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// pkgA: WhatRepo error.
	pkgProvider := goolib.PkgSpec{
		Name:     "provider_pkg",
		Arch:     "noarch",
		Version:  "2.0.0",
		Provides: []string{"pkgA"},
	}
	rsProvider := testutil.GenGoo(t, gooDir, logDir, pkgProvider)

	// pkgB: corrupted payload.
	pkgB2 := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "2.0.0"}
	rsB2 := testutil.GenGoo(t, gooDir, logDir, pkgB2)
	pkgBPath := filepath.Join(gooDir, rsB2.Source)
	if err := os.WriteFile(pkgBPath, []byte("corrupted payload"), 0644); err != nil {
		t.Fatalf("corrupting pkgB: %v", err)
	}

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsProvider, rsB2})
	if err != nil {
		t.Fatalf("json.Marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "index"), indexBytes, 0644); err != nil {
		t.Fatalf("writing index: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(indexBytes); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}
	gw.Close()
	if err := os.WriteFile(filepath.Join(gooDir, "index.gz"), gzBuf.Bytes(), 0644); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}

	cmd := &updateCmd{sources: srv.URL}
	exitStatus := cmd.Execute(ctx, nil)

	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	for _, name := range []string{"pkgA", "pkgB"} {
		ps, err := db.FetchPkg(goolib.PackageInfo{Name: name, Arch: "noarch"})
		if err != nil {
			t.Fatalf("db.FetchPkg(%s): %v", name, err)
		}
		if ps.PackageSpec == nil || ps.PackageSpec.Version != "1.0.0" {
			t.Errorf("%s version got %v, want 1.0.0 (must not be modified)", name, ps.PackageSpec)
		}
	}
}

func TestBatchUpdateContinuation_ThreePackages_MiddleSucceeds(t *testing.T) {
	// Verifies batch update continuation across three packages where first and third fail and middle succeeds.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}

	initialState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkgC", Arch: "noarch", Version: "1.0.0"}},
	}
	if err := db.WriteStateToDB(initialState); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// pkgA: installer failure on update.
	pkgA2 := goolib.PkgSpec{
		Name:    "pkgA",
		Arch:    "noarch",
		Version: "2.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_a.exe"},
	}
	rsA2 := testutil.GenGoo(t, gooDir, logDir, pkgA2)

	// pkgB: valid update.
	pkgB2 := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "2.0.0"}
	rsB2 := testutil.GenGoo(t, gooDir, logDir, pkgB2)

	// pkgC: installer failure on update.
	pkgC2 := goolib.PkgSpec{
		Name:    "pkgC",
		Arch:    "noarch",
		Version: "2.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_c.exe"},
	}
	rsC2 := testutil.GenGoo(t, gooDir, logDir, pkgC2)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsA2, rsB2, rsC2})
	if err != nil {
		t.Fatalf("json.Marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "index"), indexBytes, 0644); err != nil {
		t.Fatalf("writing index: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(indexBytes); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}
	gw.Close()
	if err := os.WriteFile(filepath.Join(gooDir, "index.gz"), gzBuf.Bytes(), 0644); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}

	cmd := &updateCmd{sources: srv.URL}
	exitStatus := cmd.Execute(ctx, nil)

	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB updated to 2.0.0.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil || psB.PackageSpec.Version != "2.0.0" {
		t.Errorf("pkgB version got %v, want 2.0.0", psB.PackageSpec)
	}

	// Verify pkgA remained at 1.0.0.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec == nil || psA.PackageSpec.Version != "1.0.0" {
		t.Errorf("pkgA version got %v, want 1.0.0", psA.PackageSpec)
	}

	// Verify pkgC remained at 1.0.0.
	psC, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgC", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgC): %v", err)
	}
	if psC.PackageSpec == nil || psC.PackageSpec.Version != "1.0.0" {
		t.Errorf("pkgC version got %v, want 1.0.0", psC.PackageSpec)
	}
}

func TestBatchUpdateContinuation_FileRollbackOnInstallFailure(t *testing.T) {
	// Verifies that when an update installer fails, the database retains the original package version,
	// and subsequent package updates continue and succeed.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}

	initialState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "pkg_rollback", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "pkg_success", Arch: "noarch", Version: "1.0.0"}},
	}
	if err := db.WriteStateToDB(initialState); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// pkg_rollback update has failing installer.
	pkgA2 := goolib.PkgSpec{
		Name:    "pkg_rollback",
		Arch:    "noarch",
		Version: "2.0.0",
		Install: goolib.ExecFile{Path: "nonexistent_installer.exe"},
	}
	rsA2 := testutil.GenGoo(t, gooDir, logDir, pkgA2)

	// pkg_success update is valid.
	pkgB2 := goolib.PkgSpec{Name: "pkg_success", Arch: "noarch", Version: "2.0.0"}
	rsB2 := testutil.GenGoo(t, gooDir, logDir, pkgB2)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsA2, rsB2})
	if err != nil {
		t.Fatalf("json.Marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "index"), indexBytes, 0644); err != nil {
		t.Fatalf("writing index: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(indexBytes); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}
	gw.Close()
	if err := os.WriteFile(filepath.Join(gooDir, "index.gz"), gzBuf.Bytes(), 0644); err != nil {
		t.Fatalf("writing index.gz: %v", err)
	}

	cmd := &updateCmd{sources: srv.URL}
	exitStatus := cmd.Execute(ctx, nil)

	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// 1. Verify pkg_rollback in DB remains at version 1.0.0.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkg_rollback", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkg_rollback): %v", err)
	}
	if psA.PackageSpec == nil || psA.PackageSpec.Version != "1.0.0" {
		t.Errorf("pkg_rollback version in DB got %v, want 1.0.0", psA.PackageSpec)
	}

	// 2. Verify pkg_success in DB is updated to version 2.0.0.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkg_success", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkg_success): %v", err)
	}
	if psB.PackageSpec == nil || psB.PackageSpec.Version != "2.0.0" {
		t.Errorf("pkg_success version in DB got %v, want 2.0.0", psB.PackageSpec)
	}
}
