package remove

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

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
				rs := testutil.GenGoo(t, gooDir, logDir, *ps.PackageSpec)
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
			if err := cmd.removeOne(t.Context(), tc.pkgName, remove.RunUninstaller, downloader, db); err != nil {
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

// captureStdio runs fn and returns what it wrote to stdout and stderr. Both
// pipes are drained while fn runs, so a large write can't block it, and both
// are restored even if fn stops the test.
func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	capture := func(f **os.File) (restore func() string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		orig := *f
		*f = w
		done := make(chan string, 1)
		go func() {
			b, _ := io.ReadAll(r)
			r.Close()
			done <- string(b)
		}()
		var once sync.Once
		var out string
		return func() string {
			once.Do(func() {
				w.Close()
				*f = orig
				out = <-done
			})
			return out
		}
	}
	restoreOut := capture(&os.Stdout)
	defer restoreOut()
	restoreErr := capture(&os.Stderr)
	defer restoreErr()
	fn()
	return restoreOut(), restoreErr()
}

func TestRemoveOneDryRun(t *testing.T) {
	logger.Init("GooGet", true, false, io.Discard)
	ctx := context.Background()

	for _, tc := range []struct {
		desc     string
		pkgName  string
		state    client.GooGetState
		wantStrs []string
		notStrs  []string
	}{
		{
			desc:    "simple remove dry run",
			pkgName: "A",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}},
				{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "2"}},
			},
			wantStrs: []string{
				"Dry run: The following packages would be removed:",
				"  A.noarch 1",
				"Dry run: Would remove A",
			},
			notStrs: []string{"Removing ", "Removal of A completed"},
		},
		{
			desc:    "remove with dependents dry run",
			pkgName: "B",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}}},
				{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"}},
			},
			wantStrs: []string{
				"Dry run: The following packages would be removed:",
				"  A.noarch 1",
				"  B.noarch 1",
				"Dry run: Would remove B",
			},
		},
		{
			desc:    "remove not installed dry run",
			pkgName: "C",
			state: client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}},
			},
			wantStrs: []string{},
			notStrs:  []string{"Dry run: The following packages would be removed:"},
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
				t.Fatalf("db.WriteStateToDB: %v", err)
			}
			// Read back the state to get the InstallDate values set by the DB.
			initialState, err := db.FetchPkgs("")
			if err != nil {
				t.Fatalf("db.FetchPkgs: %v", err)
			}

			downloader, err := client.NewDownloader("")
			if err != nil {
				t.Fatalf("client.NewDownloader: %v", err)
			}
			cmd := removeCmd{dryRun: true}

			output, _ := captureStdio(t, func() {
				if err := cmd.removeOne(ctx, tc.pkgName, remove.RunUninstaller, downloader, db); err != nil {
					t.Errorf("removeOne(%q, dryRun: true) returned error: %v", tc.pkgName, err)
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

func TestMode(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		dbOnly, skipUninstaller bool
		want                    remove.Mode
		wantErr                 bool
	}{
		{name: "no flags", want: remove.RunUninstaller},
		{name: "skip_uninstaller", skipUninstaller: true, want: remove.SkipUninstaller},
		{name: "db_only", dbOnly: true, want: remove.DBOnly},
		{name: "both", dbOnly: true, skipUninstaller: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := removeCmd{dbOnly: tc.dbOnly, skipUninstaller: tc.skipUninstaller}
			got, err := cmd.mode()
			if (err != nil) != tc.wantErr {
				t.Fatalf("mode() error = %v, want error: %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("mode() = %v, want %v", got, tc.want)
			}
		})
	}
}

// -db_only and -skip_uninstaller together are a usage error, reported before
// the database is opened.
func TestExecuteRejectsDBOnlyWithSkipUninstaller(t *testing.T) {
	logger.Init("GooGet", true, false, io.Discard)
	settings.Initialize(t.TempDir(), false)
	cmd := &removeCmd{}
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)
	if err := fs.Parse([]string{"-db_only", "-skip_uninstaller", "foo"}); err != nil {
		t.Fatal(err)
	}
	if got := cmd.Execute(t.Context(), fs); got != subcommands.ExitUsageError {
		t.Errorf("Execute() = %v, want ExitUsageError (%v)", got, subcommands.ExitUsageError)
	}
	if _, err := os.Stat(settings.DBFile()); !os.IsNotExist(err) {
		t.Errorf("Execute() created the database %s (stat error: %v)", settings.DBFile(), err)
	}
}

func TestUnavailableHint(t *testing.T) {
	unavailable := fmt.Errorf("%w: bar.noarch.1 is not cached", remove.ErrPackageUnavailable)
	for _, tc := range []struct {
		name     string
		err      error
		wantHint bool
	}{
		{"unavailable", unavailable, true},
		{"wrapped again", fmt.Errorf("removing bar: %w", unavailable), true},
		{"other error", errors.New("package not found in state file"), false},
		{"error naming the sentinel text", errors.New(remove.ErrPackageUnavailable.Error()), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unavailableHint("foo", tc.err)
			if !tc.wantHint {
				if got != "" {
					t.Errorf("unavailableHint(%v) = %q, want \"\"", tc.err, got)
				}
				return
			}
			for _, want := range []string{"googet remove -skip_uninstaller foo", "depend"} {
				if !strings.Contains(got, want) {
					t.Errorf("unavailableHint(%v) = %q, want it to contain %q", tc.err, got, want)
				}
			}
		})
	}
}

// When a package's uninstall command needs the package and it can't be
// downloaded, Execute fails and tells the user on stderr how to remove it
// without running the uninstall command.
func TestExecutePrintsUnavailableHint(t *testing.T) {
	logger.Init("GooGet", false, false, io.Discard)
	settings.Initialize(t.TempDir(), false)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	base := "foo.noarch.1.goo"
	if err := db.WriteStateToDB(client.GooGetState{{
		PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1", Uninstall: goolib.ExecFile{Path: "uninstall.sh"}},
		LocalPath:   filepath.Join(t.TempDir(), base), // Not cached.
		DownloadURL: srv.URL + "/" + base,
		Checksum:    "0000",
	}}); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	db.Close()

	cmd := &removeCmd{}
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)
	if err := fs.Parse([]string{"foo"}); err != nil {
		t.Fatal(err)
	}
	var status subcommands.ExitStatus
	_, stderr := captureStdio(t, func() { status = cmd.Execute(t.Context(), fs) })
	if status != subcommands.ExitFailure {
		t.Errorf("Execute() = %v, want ExitFailure (%v)", status, subcommands.ExitFailure)
	}
	if requests.Load() == 0 {
		t.Error("Execute() did not try to download the package")
	}
	if want := unavailableHint("foo", remove.ErrPackageUnavailable); !strings.Contains(stderr, want) {
		t.Errorf("Execute() wrote %q to stderr, want it to contain the hint %q", stderr, want)
	}

	db, err = googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	ps, err := db.FetchPkg(goolib.PackageInfo{Name: "foo", Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg: %v", err)
	}
	if ps.PackageSpec == nil {
		t.Error("Execute() removed foo from the database, want it kept")
	}
}
