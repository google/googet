package install

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

// checkInstalled returns true if the test package identified by ps was
// installed, based on whether or not the package file was written.
func checkInstalled(t *testing.T, dir string, ps goolib.PkgSpec) bool {
	t.Helper()
	filename := filepath.Join(dir, ps.Name)
	b, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("checkInstalled: error reading %q: %v", filename, err)
	}
	if got, want := string(b), ps.String(); got != want {
		t.Fatalf("checkInstalled: %q content got %v, want %v", filename, got, want)
	}
	return true
}

func TestInstall(t *testing.T) {
	logger.Init("GooGet", true, false, io.Discard)
	for _, tc := range []struct {
		desc            string             // description of test case
		args            []string           // args to install command
		state           client.GooGetState // initial DB package state
		packages        []goolib.PkgSpec   // which packages to provide in repo map
		shouldReinstall bool               // whether to reinstall
		wantInstalled   []string           // which packages were actually installed
		wantState       []string           // abbreviated final DB package state
	}{
		{
			desc: "single-install",
			args: []string{"A"},
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "3"}},
			},
			packages:      []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			wantInstalled: []string{"A.noarch.1"},
			wantState:     []string{"A.noarch.1", "C.noarch.3"},
		},
		{
			desc: "no-reinstall-when-already-installed",
			args: []string{"A", "B"},
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}},
			},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "B", Arch: "noarch", Version: "2"},
			},
			wantInstalled: []string{"B.noarch.2"},
			wantState:     []string{"A.noarch.1", "B.noarch.2"},
		},
		{
			desc: "force-reinstall-when-already-installed",
			args: []string{"A"},
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}},
			},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
			},
			shouldReinstall: true,
			wantInstalled:   []string{"A.noarch.1"},
			wantState:       []string{"A.noarch.1"},
		},
		{
			desc: "no-reinstall-when-not-installed",
			args: []string{"A"},
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "3"}},
			},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
			},
			shouldReinstall: true,
			wantState:       []string{"C.noarch.3"},
		},
		{
			desc: "no-reinstall-deps-when-already-installed",
			args: []string{"A"},
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "2"}},
				{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "3"}},
			},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "2"}},
				{Name: "B", Arch: "noarch", Version: "2"},
			},
			wantInstalled: []string{"A.noarch.1"},
			wantState:     []string{"A.noarch.1", "B.noarch.2", "C.noarch.3"},
		},
		{
			desc: "remove-replaced-package",
			args: []string{"B"},
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "5"}},
			},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "5"},
				{Name: "B", Arch: "noarch", Version: "2", Replaces: []string{"A.noarch.3"}},
			},
			wantInstalled: []string{"B.noarch.2"},
			wantState:     []string{"B.noarch.2"},
		},
		{
			desc: "remove-replaced-package-with-deps",
			args: []string{"B"},
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "5"}},
				{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "3", PkgDependencies: map[string]string{"A": "5"}}},
			},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "5"},
				{Name: "B", Arch: "noarch", Version: "2", Replaces: []string{"A.noarch.3"}},
				{Name: "C", Arch: "noarch", Version: "3", PkgDependencies: map[string]string{"A": "5"}},
			},
			wantInstalled: []string{"B.noarch.2"},
			wantState:     []string{"B.noarch.2"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			// Set up the installer.
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
			i := installer{
				db:              db,
				cache:           t.TempDir(),
				downloader:      downloader,
				shouldReinstall: tc.shouldReinstall,
			}
			// Set up the test server.
			gooDir, logDir := t.TempDir(), t.TempDir()
			srv := testutil.ServeGoo(t, gooDir)
			defer srv.Close()
			// Set up the test goo packages.
			var specs []goolib.RepoSpec
			stateMap := make(map[string]client.PackageState)
			for _, ps := range tc.state {
				stateMap[ps.PackageSpec.String()] = ps
			}
			for _, pkg := range tc.packages {
				rs := testutil.GenGoo(t, gooDir, logDir, pkg)
				specs = append(specs, rs)
				// If this package was also in the installed package state, then fill in
				// missing fields in the package state (for reinstalls).
				key := rs.PackageSpec.String()
				ps, ok := stateMap[key]
				if !ok {
					continue
				}
				ps.PackageSpec = rs.PackageSpec // fixes Files
				if ps.DownloadURL, err = url.JoinPath(srv.URL, "..", rs.Source); err != nil {
					t.Fatalf("url.JoinPath: %v", err)
				}
				ps.LocalPath = filepath.Join(i.cache, key+".goo")
				ps.Checksum = rs.Checksum
				stateMap[key] = ps
			}
			if err := db.WriteStateToDB(slices.Collect(maps.Values(stateMap))); err != nil {
				t.Fatalf("db.WriteStateToDB: %v", err)
			}
			// Initialize the installer's repo map.
			i.repoMap = client.RepoMap{srv.URL: client.Repo{Priority: priority.Default, Packages: specs}}
			// Install everything.
			archs := []string{"noarch"}
			for _, arg := range tc.args {
				if err := i.installFromRepo(context.Background(), arg, archs); err != nil {
					t.Fatalf("installFromRepo: %v", err)
				}
			}
			// Check that expected installs occurred.
			for _, pkg := range tc.packages {
				if got, want := checkInstalled(t, logDir, pkg), slices.Contains(tc.wantInstalled, pkg.String()); got != want {
					t.Fatalf("package %q installed got: %v, want: %v", pkg, got, want)
				}
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
			if diff := cmp.Diff(tc.wantState, gotState, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Fatalf("unexpected db state (-want +got):\n%v", diff)
			}
		})
	}
}

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

func TestInstallDryRun(t *testing.T) {
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()
	settings.Archs = []string{"noarch", "x86_64"}

	for _, tc := range []struct {
		desc     string
		args     []string
		state    client.GooGetState
		packages []goolib.PkgSpec
		wantStrs []string
		notStrs  []string
	}{
		{
			desc: "single package not installed",
			args: []string{"A"},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1.0.0"},
			},
			wantStrs: []string{
				"The following packages will be installed:", // Changed
				"A.noarch.1.0.0",
				"Dry run: Would install A.noarch.1.0.0 and its dependencies if not already installed.",
			},
			notStrs: []string{"Installing "},
		},
		{
			desc: "package already installed",
			args: []string{"A"},
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1.0.0"}},
			},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1.0.0"},
			},
			wantStrs: []string{"A.noarch.1.0.0 or a newer version is already installed"},
			notStrs:  []string{"The following packages will be installed:"},
		},
		{
			desc: "package with dependencies",
			args: []string{"A"},
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"B": "2.0.0"}},
				{Name: "B", Arch: "noarch", Version: "2.0.0"},
			},
			wantStrs: []string{
				"The following packages will be installed:", // Changed
				"A.noarch.1.0.0",
				"B.noarch.2.0.0",
				"Dry run: Would install A.noarch.1.0.0 and its dependencies if not already installed.",
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			if err := db.WriteStateToDB(tc.state); err != nil {
				t.Fatalf("Failed to write initial state to DB: %v", err)
			}
			// Read back the state to get the InstallDate values set by the DB.
			initialState, _ := db.FetchPkgs("")

			downloader, _ := client.NewDownloader("")
			i := &installer{
				db:         db,
				cache:      t.TempDir(),
				downloader: downloader,
				dryRun:     true,
			}

			gooDir, logDir := t.TempDir(), t.TempDir()
			srv := testutil.ServeGoo(t, gooDir)
			defer srv.Close()

			var specs []goolib.RepoSpec
			for _, pkg := range tc.packages {
				specs = append(specs, testutil.GenGoo(t, gooDir, logDir, pkg))
			}
			i.repoMap = client.RepoMap{srv.URL: client.Repo{Packages: specs}}

			cmd := installCmd{dryRun: true}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			cmd.SetFlags(fs)
			fs.Parse(tc.args)

			output := captureStdout(func() {
				for _, arg := range tc.args {
					if err := i.installFromRepo(ctx, arg, []string{"noarch"}); err != nil {
						t.Errorf("installFromRepo(%q, dryRun: true) returned error: %v", arg, err)
					}
				}
			})

			for _, want := range tc.wantStrs {
				if !strings.Contains(output, want) {
					t.Errorf("Expected stdout to contain %q, got:\n%s", want, output)
				}
			}
			for _, not := range tc.notStrs {
				if strings.Contains(output, not) {
					t.Errorf("Expected stdout NOT to contain %q, got:\n%s", not, output)
				}
			}

			// Verify DB state hasn't changed.
			finalState, err := db.FetchPkgs("")
			if err != nil {
				t.Errorf("db.FetchPkgs: %v", err)
			}
			ignoreInstallDate := cmpopts.IgnoreFields(client.PackageState{}, "InstallDate")
			if diff := cmp.Diff(finalState, initialState, cmpopts.EquateEmpty(), ignoreInstallDate); diff != "" {
				t.Errorf("DB state changed unexpectedly in dry_run (-got +want):\n%s", diff)
			}
		})
	}
}

func TestBatchInstallContinuation(t *testing.T) {
	// Verify that running googet install on multiple packages where one
	// package fails or aborts still installs the remaining packages and
	// latches exit code 1 (ExitFailure).
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// Create valid package B.
	pkgB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}
	rsB := testutil.GenGoo(t, gooDir, logDir, pkgB)

	// Write repo index and index.gz to gooDir so AvailableVersions succeeds over HTTP.
	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsB})
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

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	// "pkgA" does not exist in the repo (will fail version resolution).
	// "pkgB" exists in the repo (must succeed).
	args := []string{"-sources=" + srv.URL, "pkgA", "pkgB"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB was installed and recorded in googet.db.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil {
		t.Errorf("pkgB was not recorded in googet.db; expected successful installation")
	}
	if !checkInstalled(t, logDir, pkgB) {
		t.Errorf("pkgB file was not installed to target directory")
	}

	// Verify pkgA was NOT recorded in googet.db.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec != nil {
		t.Errorf("pkgA was recorded in googet.db; expected failure")
	}
}

func TestBatchInstallContinuation_InstallerExecutionFailure(t *testing.T) {
	// Verifies batch continuation when package A downloads but fails installer execution.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// Create package A with a failing installer execution path.
	pkgA := goolib.PkgSpec{
		Name:    "pkgA",
		Arch:    "noarch",
		Version: "1.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_binary.exe"},
	}
	rsA := testutil.GenGoo(t, gooDir, logDir, pkgA)

	// Create package B which is completely valid.
	pkgB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}
	rsB := testutil.GenGoo(t, gooDir, logDir, pkgB)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsA, rsB})
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

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	args := []string{"-sources=" + srv.URL, "pkgA", "pkgB"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB was installed and recorded in googet.db.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil {
		t.Errorf("pkgB was not recorded in googet.db; expected successful installation")
	}
	if !checkInstalled(t, logDir, pkgB) {
		t.Errorf("pkgB file was not installed to target directory")
	}

	// Verify pkgA was NOT recorded in googet.db.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec != nil {
		t.Errorf("pkgA was recorded in googet.db; expected failure")
	}
}

func TestBatchInstallContinuation_OrderReversal(t *testing.T) {
	// Verifies batch continuation when successful package precedes failing package.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	pkgA := goolib.PkgSpec{
		Name:    "pkgA",
		Arch:    "noarch",
		Version: "1.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_binary.exe"},
	}
	rsA := testutil.GenGoo(t, gooDir, logDir, pkgA)

	pkgB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}
	rsB := testutil.GenGoo(t, gooDir, logDir, pkgB)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsA, rsB})
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

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	// pkgB (success) comes first, pkgA (fail) comes second.
	args := []string{"-sources=" + srv.URL, "pkgB", "pkgA"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB was installed and recorded in googet.db.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil {
		t.Errorf("pkgB was not recorded in googet.db; expected successful installation")
	}
	if !checkInstalled(t, logDir, pkgB) {
		t.Errorf("pkgB file was not installed to target directory")
	}

	// Verify pkgA was NOT recorded in googet.db.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec != nil {
		t.Errorf("pkgA was recorded in googet.db; expected failure")
	}
}

func TestBatchInstallContinuation_LocalGooFiles(t *testing.T) {
	// Verifies batch continuation when installing local .goo files where one fails.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	pkgDir, logDir := t.TempDir(), t.TempDir()

	pkgA := goolib.PkgSpec{
		Name:    "pkgA",
		Arch:    "noarch",
		Version: "1.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_binary.exe"},
	}
	testutil.GenGoo(t, pkgDir, logDir, pkgA)
	fileA := filepath.Join(pkgDir, pkgA.String()+".goo")

	pkgB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}
	testutil.GenGoo(t, pkgDir, logDir, pkgB)
	fileB := filepath.Join(pkgDir, pkgB.String()+".goo")

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	args := []string{fileA, fileB}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB was installed and recorded in googet.db.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil {
		t.Errorf("pkgB was not recorded in googet.db; expected successful installation")
	}
	if !checkInstalled(t, logDir, pkgB) {
		t.Errorf("pkgB file was not installed to target directory")
	}

	// Verify pkgA was NOT recorded in googet.db.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec != nil {
		t.Errorf("pkgA was recorded in googet.db; expected failure")
	}
}

func TestBatchInstallContinuation_MixedFileAndRepo(t *testing.T) {
	// Verifies batch continuation when mixing local .goo files and repo packages.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	localDir, gooDir, logDir := t.TempDir(), t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	// Local file pkgA that fails installer execution.
	pkgA := goolib.PkgSpec{
		Name:    "pkgA",
		Arch:    "noarch",
		Version: "1.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_binary.exe"},
	}
	testutil.GenGoo(t, localDir, logDir, pkgA)
	fileA := filepath.Join(localDir, pkgA.String()+".goo")

	// Repo package pkgB that succeeds.
	pkgB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}
	rsB := testutil.GenGoo(t, gooDir, logDir, pkgB)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsB})
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

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	args := []string{"-sources=" + srv.URL, fileA, "pkgB"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil {
		t.Errorf("pkgB was not recorded in googet.db; expected successful installation")
	}

	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec != nil {
		t.Errorf("pkgA was recorded in googet.db; expected failure")
	}
}

func TestBatchInstall_AllSucceed(t *testing.T) {
	// Verifies batch install returns ExitSuccess when all packages succeed.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	pkgB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}
	rsB := testutil.GenGoo(t, gooDir, logDir, pkgB)

	pkgC := goolib.PkgSpec{Name: "pkgC", Arch: "noarch", Version: "1.0.0"}
	rsC := testutil.GenGoo(t, gooDir, logDir, pkgC)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsB, rsC})
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

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	args := []string{"-sources=" + srv.URL, "pkgB", "pkgC"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitSuccess {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitSuccess (%v)", exitStatus, subcommands.ExitSuccess)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	for _, name := range []string{"pkgB", "pkgC"} {
		ps, err := db.FetchPkg(goolib.PackageInfo{Name: name, Arch: "noarch"})
		if err != nil {
			t.Fatalf("db.FetchPkg(%s): %v", name, err)
		}
		if ps.PackageSpec == nil {
			t.Errorf("package %s was not recorded in googet.db", name)
		}
	}
}

func TestBatchInstall_AllFail(t *testing.T) {
	// Verifies batch install returns ExitFailure when all packages fail.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	gooDir := t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	args := []string{"-sources=" + srv.URL, "nonexistentA", "nonexistentB"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	pkgs, err := db.FetchPkgs("")
	if err != nil {
		t.Fatalf("db.FetchPkgs: %v", err)
	}
	if len(pkgs) != 0 {
		t.Errorf("expected 0 packages in db, got %d", len(pkgs))
	}
}

func TestBatchInstallContinuation_ThreePackages_MiddleSucceeds(t *testing.T) {
	// Verifies batch continuation across three packages where first and third fail and middle succeeds.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	pkgA := goolib.PkgSpec{
		Name:    "pkgA",
		Arch:    "noarch",
		Version: "1.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_a.exe"},
	}
	rsA := testutil.GenGoo(t, gooDir, logDir, pkgA)

	pkgB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"}
	rsB := testutil.GenGoo(t, gooDir, logDir, pkgB)

	pkgC := goolib.PkgSpec{
		Name:    "pkgC",
		Arch:    "noarch",
		Version: "1.0.0",
		Install: goolib.ExecFile{Path: "failing_installer_c.exe"},
	}
	rsC := testutil.GenGoo(t, gooDir, logDir, pkgC)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsA, rsB, rsC})
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

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	args := []string{"-sources=" + srv.URL, "pkgA", "pkgB", "pkgC"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	// Verify pkgB is in DB and installed.
	psB, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgB): %v", err)
	}
	if psB.PackageSpec == nil {
		t.Errorf("pkgB was not recorded in googet.db; expected successful installation")
	}
	if !checkInstalled(t, logDir, pkgB) {
		t.Errorf("pkgB file was not installed to target directory")
	}

	// Verify pkgA is not in DB and placed file rolled back.
	psA, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgA", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgA): %v", err)
	}
	if psA.PackageSpec != nil {
		t.Errorf("pkgA was recorded in googet.db; expected failure")
	}
	if _, err := os.Stat(filepath.Join(logDir, pkgA.Name)); !os.IsNotExist(err) {
		t.Errorf("pkgA placed file still exists on disk; expected rollback deletion")
	}

	// Verify pkgC is not in DB and placed file rolled back.
	psC, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgC", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg(pkgC): %v", err)
	}
	if psC.PackageSpec != nil {
		t.Errorf("pkgC was recorded in googet.db; expected failure")
	}
	if _, err := os.Stat(filepath.Join(logDir, pkgC.Name)); !os.IsNotExist(err) {
		t.Errorf("pkgC placed file still exists on disk; expected rollback deletion")
	}
}

func TestBatchInstallContinuation_RollbackUnlinksPlacedFile(t *testing.T) {
	// Verifies that when package installation fails, newly placed files are unlinked from disk.
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	settings.Initialize(t.TempDir(), false)
	settings.Archs = []string{"noarch"}
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatalf("os.MkdirAll cache: %v", err)
	}

	gooDir, logDir := t.TempDir(), t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	defer srv.Close()

	pkgA := goolib.PkgSpec{
		Name:    "pkg_rollback_test",
		Arch:    "noarch",
		Version: "1.0.0",
		Install: goolib.ExecFile{Path: "failing_binary.exe"},
	}
	rsA := testutil.GenGoo(t, gooDir, logDir, pkgA)

	pkgB := goolib.PkgSpec{Name: "pkg_ok_test", Arch: "noarch", Version: "1.0.0"}
	rsB := testutil.GenGoo(t, gooDir, logDir, pkgB)

	indexBytes, err := json.Marshal([]goolib.RepoSpec{rsA, rsB})
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

	cmd := &installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)

	args := []string{"-sources=" + srv.URL, "pkg_rollback_test", "pkg_ok_test"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	exitStatus := cmd.Execute(ctx, fs)
	if exitStatus != subcommands.ExitFailure {
		t.Errorf("cmd.Execute got %v, want subcommands.ExitFailure (%v)", exitStatus, subcommands.ExitFailure)
	}

	// Verify pkg_rollback_test file was deleted.
	placedFile := filepath.Join(logDir, pkgA.Name)
	if _, err := os.Stat(placedFile); !os.IsNotExist(err) {
		t.Errorf("Placed file %s still exists; expected rollback removal", placedFile)
	}

	// Verify pkg_ok_test file exists.
	okFile := filepath.Join(logDir, pkgB.Name)
	if _, err := os.Stat(okFile); err != nil {
		t.Errorf("Placed file %s does not exist: %v", okFile, err)
	}
}
