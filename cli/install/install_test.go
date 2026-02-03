package install

import (
	"bytes"
	"context"
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

			// Verify DB state hasn't changed
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
