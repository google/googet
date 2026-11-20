package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/remove"
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

func writeTestGoo(t *testing.T, dir string, ps goolib.PkgSpec, files map[string][]byte, modes map[string]int64) string {
	t.Helper()
	specJSON, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	gooPath := filepath.Join(dir, ps.String()+".goo")
	f, err := os.Create(gooPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for name, body := range files {
		mode := modes[name]
		if mode == 0 {
			mode = 0644
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: ps.Name + ".pkgspec", Mode: 0644, Size: int64(len(specJSON))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(specJSON); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return gooPath
}

func TestInstallStopsOnInterrupt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux shell script and Unix signal forwarding")
	}
	const envKey = "GOOGET_CLI_INSTALL_INTERRUPT"
	if os.Getenv(envKey) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestInstallStopsOnInterrupt$")
		cmd.Env = append(os.Environ(), envKey+"=1", "GORACE=atexit_sleep_ms=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("subprocess failed: %v\n%s", err, out)
		}
		return
	}

	var logBuf bytes.Buffer
	logger.Init("GooGet", false, false, &logBuf)
	settings.Initialize(t.TempDir(), false)
	if err := os.MkdirAll(settings.CacheDir(), 0755); err != nil {
		t.Fatal(err)
	}
	gooDir, dstDir := t.TempDir(), t.TempDir()
	aGoo := writeTestGoo(t, gooDir,
		goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1.0.0", Install: goolib.ExecFile{Path: "install.sh"}},
		map[string][]byte{"install.sh": []byte("#!/bin/sh\nkill -TERM $PPID\nsleep 10\n")},
		map[string]int64{"install.sh": 0755},
	)
	bTarget := filepath.Join(dstDir, "bfile")
	bGoo := writeTestGoo(t, gooDir,
		goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1.0.0", Files: map[string]string{"bfile": bTarget}},
		map[string][]byte{"bfile": []byte("payload")},
		nil,
	)

	cmd := installCmd{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cmd.SetFlags(fs)
	if err := fs.Parse([]string{aGoo, bGoo}); err != nil {
		t.Fatal(err)
	}
	if got := cmd.Execute(t.Context(), fs); got != subcommands.ExitFailure {
		t.Errorf("Execute() = %v, want %v", got, subcommands.ExitFailure)
	}
	if strings.Contains(logBuf.String(), "B.noarch.1.0.0.goo") {
		t.Errorf("Execute() attempted second package after interrupt:\n%s", logBuf.String())
	}
	if _, err := os.Stat(filepath.Join(settings.CacheDir(), "B.noarch.1.0.0.goo")); !os.IsNotExist(err) {
		t.Errorf("second package cached after interrupt (stat err = %v)", err)
	}
	if _, err := os.Stat(bTarget); !os.IsNotExist(err) {
		t.Errorf("second package file %q created after interrupt (stat err = %v)", bTarget, err)
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if state, err := db.FetchPkgs(""); err != nil || len(state) != 0 {
		t.Errorf("FetchPkgs() = %v, %v, want empty state", state, err)
	}

	// Direct package-level calls must also fail fast without copying files,
	// downloading, or removing installed packages once goolib.Interrupted() is set.
	t.Run("FromDisk", func(t *testing.T) {
		diskCache := t.TempDir()
		if err := install.FromDisk(bGoo, diskCache, false, false, false, db); !errors.Is(err, goolib.ErrInterrupted) {
			t.Errorf("FromDisk() after interrupt = %v, want %v", err, goolib.ErrInterrupted)
		}
		if entries, err := os.ReadDir(diskCache); err != nil || len(entries) != 0 {
			t.Errorf("FromDisk() populated cache after interrupt: entries = %v, err = %v", entries, err)
		}
		if _, err := os.Stat(bTarget); !os.IsNotExist(err) {
			t.Errorf("FromDisk() copied %q after interrupt (stat err = %v)", bTarget, err)
		}
	})
	t.Run("FromRepo", func(t *testing.T) {
		rm := client.RepoMap{
			"http://127.0.0.1:0": client.Repo{
				Packages: []goolib.RepoSpec{{
					PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "1.0.0"},
				}},
			},
		}
		if err := install.FromRepo(t.Context(), goolib.PackageInfo{Name: "C", Arch: "noarch", Ver: "1.0.0"}, "http://127.0.0.1:0", t.TempDir(), rm, []string{"noarch"}, false, false, nil, db); !errors.Is(err, goolib.ErrInterrupted) {
			t.Errorf("FromRepo() after interrupt = %v, want %v", err, goolib.ErrInterrupted)
		}
	})
	t.Run("Reinstall", func(t *testing.T) {
		reinstallState := client.PackageState{
			LocalPath:   filepath.Join(gooDir, "missing.goo"),
			DownloadURL: "http://127.0.0.1:0/B.goo",
			PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1.0.0"},
		}
		if err := install.Reinstall(t.Context(), reinstallState, true, false, nil, db); !errors.Is(err, goolib.ErrInterrupted) {
			t.Errorf("Reinstall() after interrupt = %v, want %v", err, goolib.ErrInterrupted)
		}
	})
	t.Run("RemoveAll", func(t *testing.T) {
		pFile := filepath.Join(dstDir, "pfile")
		if err := os.WriteFile(pFile, []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := db.AddPkg(client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{pFile: "sum"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := remove.All(t.Context(), goolib.PackageInfo{Name: "P", Arch: "noarch"}, remove.DepMap{"P.noarch": nil}, false, nil, db); !errors.Is(err, goolib.ErrInterrupted) {
			t.Errorf("remove.All() after interrupt = %v, want %v", err, goolib.ErrInterrupted)
		}
		if _, err := os.Stat(pFile); err != nil {
			t.Errorf("remove.All() removed %q after interrupt: %v", pFile, err)
		}
	})
}

func TestFromRepoStopsAfterWaitDelayInterrupt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux shell script and Unix signal forwarding")
	}
	const envKey = "GOOGET_FROMREPO_WAITDELAY_MODE"
	if mode := os.Getenv(envKey); mode != "" {
		logger.Init("GooGet", false, false, io.Discard)
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		downloader, err := client.NewDownloader("")
		if err != nil {
			t.Fatal(err)
		}
		gooDir, cache := t.TempDir(), t.TempDir()
		srv := testutil.ServeGoo(t, gooDir)
		defer srv.Close()

		drainSig := []byte("#!/bin/sh\n(while kill -0 $$ 2>/dev/null; do sleep 0.01; done; kill -TERM $PPID; sleep 0.5) &\nexit 0\n")
		parentSpec := goolib.PkgSpec{Name: "Parent", Arch: "noarch", Version: "1.0.0"}
		var specs []goolib.RepoSpec

		switch mode {
		case "after-dep":
			depSpec := goolib.PkgSpec{Name: "Dep", Arch: "noarch", Version: "1.0.0", Install: goolib.ExecFile{Path: "install.sh"}}
			depGoo := writeTestGoo(t, gooDir, depSpec, map[string][]byte{"install.sh": drainSig}, map[string]int64{"install.sh": 0755})
			f, err := os.Open(depGoo)
			if err != nil {
				t.Fatal(err)
			}
			depSum := goolib.Checksum(f)
			f.Close()
			parentSpec.PkgDependencies = map[string]string{"Dep": "1.0.0"}
			specs = append(specs, goolib.RepoSpec{
				PackageSpec: &depSpec,
				Source:      filepath.Base(depGoo),
				Checksum:    depSum,
			})
			t.Cleanup(func() {
				if st, err := db.FetchPkg(goolib.PackageInfo{Name: "Dep", Arch: "noarch"}); err != nil || st.PackageSpec == nil {
					t.Errorf("Dep was not recorded before post-exit interrupt: state=%v, err=%v", st, err)
				}
			})
		case "during-replacements":
			rep1Spec := goolib.PkgSpec{Name: "Rep1", Arch: "noarch", Version: "1.0.0", Uninstall: goolib.ExecFile{Path: "uninstall.sh"}}
			rep1Goo := writeTestGoo(t, cache, rep1Spec, map[string][]byte{"uninstall.sh": drainSig}, map[string]int64{"uninstall.sh": 0755})
			rep2File := filepath.Join(t.TempDir(), "rep2file")
			if err := os.WriteFile(rep2File, []byte("keep"), 0644); err != nil {
				t.Fatal(err)
			}
			for _, st := range []client.PackageState{
				{LocalPath: rep1Goo, PackageSpec: &rep1Spec},
				{PackageSpec: &goolib.PkgSpec{Name: "Rep2", Arch: "noarch", Version: "1.0.0"}, InstalledFiles: map[string]string{rep2File: "sum"}},
			} {
				if err := db.AddPkg(st); err != nil {
					t.Fatal(err)
				}
			}
			parentSpec.Replaces = []string{"Rep1", "Rep2"}
			t.Cleanup(func() {
				if st, err := db.FetchPkg(goolib.PackageInfo{Name: "Rep1", Arch: "noarch"}); err != nil || st.PackageSpec != nil {
					t.Errorf("Rep1 was not removed before post-exit interrupt: state=%v, err=%v", st, err)
				}
				if _, err := os.Stat(rep2File); err != nil {
					t.Errorf("Rep2 file %q removed after interrupt: %v", rep2File, err)
				}
			})
		}

		parentGoo := writeTestGoo(t, gooDir, parentSpec, nil, nil)
		f, err := os.Open(parentGoo)
		if err != nil {
			t.Fatal(err)
		}
		parentSum := goolib.Checksum(f)
		f.Close()
		specs = append(specs, goolib.RepoSpec{
			PackageSpec: &parentSpec,
			Source:      filepath.Base(parentGoo),
			Checksum:    parentSum,
		})
		rm := client.RepoMap{srv.URL + "/repo": client.Repo{Packages: specs}}

		err = install.FromRepo(t.Context(), goolib.PackageInfo{Name: "Parent", Arch: "noarch", Ver: "1.0.0"}, srv.URL+"/repo", cache, rm, []string{"noarch"}, false, false, downloader, db)
		if !errors.Is(err, goolib.ErrInterrupted) {
			t.Errorf("FromRepo(Parent) = %v, want %v", err, goolib.ErrInterrupted)
		}
		if _, serr := os.Stat(filepath.Join(cache, filepath.Base(parentGoo))); !os.IsNotExist(serr) {
			t.Errorf("Parent package downloaded to cache after interrupt (stat err = %v)", serr)
		}
		return
	}

	for _, mode := range []string{"after-dep", "during-replacements"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestFromRepoStopsAfterWaitDelayInterrupt$")
			cmd.Env = append(os.Environ(), envKey+"="+mode, "GORACE=atexit_sleep_ms=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("subprocess (%s) failed: %v\n%s", mode, err, out)
			}
		})
	}
}

func TestReinstallWrapsTimeout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux shell script")
	}
	logger.Init("GooGet", false, false, io.Discard)
	settings.Initialize(t.TempDir(), false)
	orig := goolib.Timeout
	t.Cleanup(func() { goolib.Timeout = orig })
	goolib.Timeout = 100 * time.Millisecond

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ps := goolib.PkgSpec{Name: "H", Arch: "noarch", Version: "1.0.0", Install: goolib.ExecFile{Path: "install.sh"}}
	goo := writeTestGoo(t, t.TempDir(), ps,
		map[string][]byte{"install.sh": []byte("#!/bin/sh\nsleep 10\n")},
		map[string]int64{"install.sh": 0755},
	)
	err = install.Reinstall(t.Context(), client.PackageState{LocalPath: goo, PackageSpec: &ps}, false, false, nil, db)
	if !errors.Is(err, goolib.ErrTimeout) {
		t.Fatalf("Reinstall() error = %v, want it to wrap %v", err, goolib.ErrTimeout)
	}
}
