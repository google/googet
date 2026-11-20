package remove

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
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

			output := captureStdout(func() {
				if err := cmd.removeOne(ctx, tc.pkgName, downloader, db); err != nil {
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

func TestDryRunRequested(t *testing.T) {
	for _, tc := range []struct {
		args string
		want bool
	}{
		{"-dry_run pkg", true},
		{"--dry_run pkg", true},
		{"-dry_run=true pkg", true},
		{"-db_only -dry_run pkg", true},
		{"-dry_run=false -dry_run pkg", true},
		{"-dry_run", true}, // Execute then refuses for lack of a package.
		{"pkg", false},
		{"", false},
		{"-dry_run=false pkg", false},
		{"-dry_run -dry_run=false pkg", false},
		{"pkg -dry_run", false}, // Flag parsing stops at the first positional argument.
		{"-- -dry_run", false},
		{"-dryrun pkg", false},
		{"-bogus -dry_run pkg", false},
		{"-dry_run -bogus pkg", false},
		{"-dry_run=maybe pkg", false},
		{"-h", false},
	} {
		t.Run(tc.args, func(t *testing.T) {
			if got := DryRunRequested(strings.Fields(tc.args)); got != tc.want {
				t.Errorf("DryRunRequested(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestExecuteRequiresPrivilege checks that without the privileged marker in
// its context, Execute only runs a dry run.
func TestExecuteRequiresPrivilege(t *testing.T) {
	logger.Init("GooGet", true, false, io.Discard)
	for _, tc := range []struct {
		desc       string
		args       []string
		privileged bool
		wantStatus subcommands.ExitStatus
		wantKept   bool // Whether package A is still installed afterwards.
	}{
		{desc: "remove without privilege", args: []string{"A"}, wantStatus: subcommands.ExitFailure, wantKept: true},
		{desc: "db_only remove without privilege", args: []string{"-db_only", "A"}, wantStatus: subcommands.ExitFailure, wantKept: true},
		{desc: "dry run without privilege", args: []string{"-dry_run", "A"}, wantStatus: subcommands.ExitSuccess, wantKept: true},
		{desc: "remove with privilege", args: []string{"A"}, privileged: true, wantStatus: subcommands.ExitSuccess, wantKept: false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			gooDir := t.TempDir()
			spec := goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}
			rs := testutil.GenGoo(t, gooDir, t.TempDir(), spec)
			state := client.GooGetState{{
				PackageSpec: rs.PackageSpec,
				LocalPath:   filepath.Join(gooDir, rs.PackageSpec.String()+".goo"),
				Checksum:    rs.Checksum,
			}}
			if err := db.WriteStateToDB(state); err != nil {
				t.Fatalf("db.WriteStateToDB: %v", err)
			}

			var cmd removeCmd
			fs := flag.NewFlagSet("remove", flag.ContinueOnError)
			cmd.SetFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parsing %q: %v", tc.args, err)
			}
			ctx := context.Background()
			if tc.privileged {
				ctx = cli.WithPrivileged(ctx)
			}
			if got := cmd.Execute(ctx, fs); got != tc.wantStatus {
				t.Errorf("Execute(%q, privileged=%v) = %v, want %v", tc.args, tc.privileged, got, tc.wantStatus)
			}
			ps, err := db.FetchPkg(goolib.PackageInfo{Name: "A", Arch: "noarch"})
			if err != nil {
				t.Fatalf("db.FetchPkg: %v", err)
			}
			if kept := ps.PackageSpec != nil; kept != tc.wantKept {
				t.Errorf("after Execute(%q, privileged=%v) package A installed = %v, want %v", tc.args, tc.privileged, kept, tc.wantKept)
			}
		})
	}
}
