/*
Copyright 2016 Google Inc. All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/system"
	"github.com/google/subcommands"
)

// Environment variables that runGooget sets for the child process.
const (
	// runArgsEnv, when set, makes the test binary run googet's run() with
	// the JSON-encoded arguments it holds instead of running tests. run()
	// registers flags on flag.CommandLine and commands on
	// subcommands.DefaultCommander, so each run needs a fresh process.
	runArgsEnv = "GOOGET_TEST_RUN_ARGS"
	// spyDirEnv names the directory where the isAdmin and obtainLock spies
	// record that they were called.
	spyDirEnv = "GOOGET_TEST_SPY_DIR"
	// allowEnv, when set, makes the admin check and the lock succeed.
	// Otherwise the child behaves as an unelevated process.
	allowEnv = "GOOGET_TEST_ALLOW_PRIVILEGE"
)

// TestMain serves all tests in package main. When runArgsEnv is set, the test
// binary acts as googet for runGooget instead of running the tests.
func TestMain(m *testing.M) {
	if args := os.Getenv(runArgsEnv); args != "" {
		os.Exit(runWithSpies(args))
	}
	os.Exit(m.Run())
}

// runWithSpies runs googet in a child process started by runGooget, with
// isAdmin and obtainLock replaced by spies that record the call. isAdmin
// fails unless allowEnv is set. obtainLock fails unless allowEnv is set, and
// also fails if the googet root doesn't exist yet, because the root must be
// created, with its own mode, before the lock file.
func runWithSpies(encodedArgs string) int {
	var args []string
	if err := json.Unmarshal([]byte(encodedArgs), &args); err != nil {
		panic(err)
	}
	spyDir := os.Getenv(spyDirEnv)
	record := func(name string) {
		if err := os.WriteFile(filepath.Join(spyDir, name), nil, 0644); err != nil {
			panic(err)
		}
	}
	allow := os.Getenv(allowEnv) != ""
	isAdmin = func() error {
		record("isAdmin")
		if allow {
			return nil
		}
		return errors.New("spy: not an administrator")
	}
	obtainLock = func(lockFile string, maxAge time.Duration) (func(), error) {
		record("obtainLock")
		if !allow {
			return nil, errors.New("spy: lock refused")
		}
		root := filepath.Dir(lockFile)
		if !isDir(root) {
			return nil, fmt.Errorf("googet root %q doesn't exist when taking the lock", root)
		}
		return system.ObtainLock(lockFile, maxAge)
	}
	os.Args = append([]string{"googet"}, args...)
	return run(context.Background())
}

type runResult struct {
	code           int
	stdout, stderr string
	spies          map[string]bool // Spies that were called.
}

// output returns everything the child wrote, stdout first.
func (r runResult) output() string {
	return r.stdout + r.stderr
}

// runGooget runs googet with args in a child process, with isAdmin and
// obtainLock replaced by spies that succeed if allow is set and fail
// otherwise. The GooGetRoot environment variable is unset, so only -root
// sets the root.
func runGooget(t *testing.T, allow bool, args ...string) runResult {
	t.Helper()
	enc, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	spyDir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(envVar)+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// The race detector otherwise sleeps for a second before each exit.
	cmd.Env = append(cmd.Env, runArgsEnv+"="+string(enc), spyDirEnv+"="+spyDir, "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	if allow {
		cmd.Env = append(cmd.Env, allowEnv+"=1")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	res := runResult{stdout: stdout.String(), stderr: stderr.String(), spies: map[string]bool{}}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("running googet %q: %v", args, err)
	}
	for _, s := range []string{"isAdmin", "obtainLock"} {
		if _, err := os.Stat(filepath.Join(spyDir, s)); err == nil {
			res.spies[s] = true
		}
	}
	return res
}

// fixtureRoot returns a googet root whose database has package A and package
// B, which depends on A.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	state := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1.0.0@1"}},
		{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1.0.0@1", PkgDependencies: map[string]string{"A": "1.0.0@1"}}},
	}
	if err := db.WriteStateToDB(state); err != nil {
		db.Close()
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
	// Backdate the database so that any later write moves its mtime forward,
	// whatever the filesystem's timestamp granularity.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "googet.db"), old, old); err != nil {
		t.Fatalf("os.Chtimes: %v", err)
	}
	return root
}

// rootSnapshot records the googet root's file names and the database's
// bytes and modification time.
type rootSnapshot struct {
	names   []string
	db      []byte
	dbMtime time.Time
}

func snapshotRoot(t *testing.T, root string) rootSnapshot {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var s rootSnapshot
	for _, e := range entries {
		s.names = append(s.names, e.Name())
	}
	dbFile := filepath.Join(root, "googet.db")
	if s.db, err = os.ReadFile(dbFile); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	s.dbMtime = fi.ModTime()
	return s
}

// installed reports whether package name is in the database under root.
func installed(t *testing.T, root, name string) bool {
	t.Helper()
	db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	ps, err := db.FetchPkg(goolib.PackageInfo{Name: name, Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg: %v", err)
	}
	return ps.PackageSpec != nil
}

// checkArgsHelperEnv, when set, makes TestRunRejectsMisplacedFlagsHelper run
// googet's run() with the JSON-encoded arguments it holds.
const checkArgsHelperEnv = "GOOGET_TEST_CHECKARGS_RUN"

// TestRunRejectsMisplacedFlagsHelper isn't a test: it runs run() in a child
// process started by TestRunRejectsMisplacedFlags.
func TestRunRejectsMisplacedFlagsHelper(t *testing.T) {
	enc := os.Getenv(checkArgsHelperEnv)
	if enc == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(enc), &args); err != nil {
		panic(err)
	}
	os.Args = append([]string{"googet"}, args...)
	os.Exit(run(context.Background()))
}

// TestRunRejectsMisplacedFlags checks that a flag after a package name, or a
// positional argument for a command that takes none, is a usage error that
// run() reports before it creates the root or runs the command.
func TestRunRejectsMisplacedFlags(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		wantOut string
	}{
		{args: []string{"install", "pkg", "-dry_run"}, wantOut: `"-dry_run" after "pkg"`},
		{args: []string{"install", "-dry_run", "pkg", "-reinstall"}, wantOut: `"-reinstall" after "pkg"`},
		{args: []string{"remove", "pkg", "-dry_run"}, wantOut: `"-dry_run" after "pkg"`},
		{args: []string{"update", "foo", "-dry_run"}, wantOut: `update takes no arguments, got "foo"`},
		{args: []string{"clean", "foo", "-all"}, wantOut: `clean takes no arguments, got "foo"`},
		{args: []string{"check", "foo"}, wantOut: `check takes no arguments, got "foo"`},
		{args: []string{"listrepos", "x"}, wantOut: `listrepos takes no arguments, got "x"`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			args := append([]string{"-root", root, "-noconfirm", "-system_log=false"}, tc.args...)
			enc, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunRejectsMisplacedFlagsHelper$")
			// Drop the root variable so the child can never fall back to a real
			// root; Windows matches variable names case-insensitively.
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(envVar)+"=") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			// atexit_sleep_ms=0 stops a -race child from sleeping 1s on exit.
			cmd.Env = append(cmd.Env, checkArgsHelperEnv+"="+string(enc), "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
			out, err := cmd.CombinedOutput()
			code := 0
			var exitErr *exec.ExitError
			switch {
			case errors.As(err, &exitErr):
				code = exitErr.ExitCode()
			case err != nil:
				t.Fatalf("running googet %q: %v", tc.args, err)
			}
			if code != 2 {
				t.Errorf("googet %q exit code = %d, want 2; output:\n%s", tc.args, code, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("googet %q output = %q, want it to contain %q", tc.args, out, tc.wantOut)
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("googet %q created the root %q (Lstat error: %v), want it untouched", tc.args, root, err)
			}
		})
	}
}

func TestRotateLog(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	table := []struct {
		name    string
		size    int64
		rotated bool
	}{
		{"test1.log", 10 * 1024, true},
		{"test2.log", 9 * 1024, false},
	}

	for _, tt := range table {
		logPath := filepath.Join(tempDir, tt.name)
		f, err := oswrap.Create(logPath)
		if err != nil {
			t.Fatalf("error creating log file: %v", err)
		}

		if err := f.Truncate(tt.size); err != nil {
			t.Fatalf("error truncating log file: %v", err)
		}

		if err := f.Close(); err != nil {
			t.Fatalf("error closing log file: %v", err)
		}

		if err := rotateLog(logPath, 10*1024); err != nil {
			t.Errorf("error running rotateLog: %v", err)
		}

		switch tt.rotated {
		case true:
			if _, err := oswrap.Stat(logPath); err == nil {
				t.Error("rotateLog did not rotate log as expected, old log file still exists")
			}
			if _, err := oswrap.Stat(logPath + ".old"); err != nil {
				t.Error("rotateLog did not rotate log as expected, .old file does not exist")
			}
		case false:
			if _, err := oswrap.Stat(logPath); err != nil {
				t.Error("rotateLog rotated a log we didn't expect")
			}
		}
	}
}

func TestWantProgress(t *testing.T) {
	for _, tc := range []struct {
		desc                            string
		conf, flagSet, flagVal, verbose bool
		want                            bool
	}{
		{desc: "defaults", conf: true, want: true},
		{desc: "config off", conf: false, want: false},
		{desc: "flag off overrides config on", conf: true, flagSet: true, flagVal: false, want: false},
		{desc: "flag on overrides config off", conf: false, flagSet: true, flagVal: true, want: true},
		{desc: "unset flag value is ignored", conf: false, flagSet: false, flagVal: true, want: false},
		{desc: "verbose disables", conf: true, verbose: true, want: false},
		{desc: "verbose beats explicit flag on", conf: true, flagSet: true, flagVal: true, verbose: true, want: false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if got := wantProgress(tc.conf, tc.flagSet, tc.flagVal, tc.verbose); got != tc.want {
				t.Errorf("wantProgress(conf=%v, flagSet=%v, flagVal=%v, verbose=%v) = %v, want %v",
					tc.conf, tc.flagSet, tc.flagVal, tc.verbose, got, tc.want)
			}
		})
	}
}

// TestClassifyCommands pins each registered command's unelevated policy:
// which of its flags make cli.Classify return UnelevatedRun. How Classify
// parses flags is tested in package cli.
func TestClassifyCommands(t *testing.T) {
	for _, tc := range []struct {
		args string
		want cli.Class
	}{
		{"installed", cli.UnelevatedRun},
		{"installed -info A", cli.UnelevatedRun},
		{"listrepos", cli.UnelevatedRun},
		{"remove -dry_run pkg", cli.UnelevatedRun},
		{"remove pkg", cli.Privileged},
		{"install -dry_run pkg", cli.Privileged},
	} {
		t.Run(tc.args, func(t *testing.T) {
			if got, _, _ := cli.Classify(subcommands.DefaultCommander, strings.Fields(tc.args)); got != tc.want {
				t.Errorf("Classify(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestAllowUnelevatedAllowlist pins the sets of registered commands that
// implement cli.AllowUnelevated and cli.LockFree, so adding one is a visible,
// reviewed change.
func TestAllowUnelevatedAllowlist(t *testing.T) {
	wantAllow := []string{"installed", "listrepos", "remove"}
	wantLockFree := []string{"installed", "listrepos"}
	var gotAllow, gotLockFree []string
	subcommands.DefaultCommander.VisitCommands(func(_ *subcommands.CommandGroup, c subcommands.Command) {
		if _, ok := c.(cli.AllowUnelevated); ok {
			gotAllow = append(gotAllow, c.Name())
		}
		if _, ok := c.(cli.LockFree); ok {
			gotLockFree = append(gotLockFree, c.Name())
		}
	})
	slices.Sort(gotAllow)
	slices.Sort(gotLockFree)
	if !slices.Equal(gotAllow, wantAllow) {
		t.Errorf("commands implementing cli.AllowUnelevated = %q, want %q", gotAllow, wantAllow)
	}
	if !slices.Equal(gotLockFree, wantLockFree) {
		t.Errorf("commands implementing cli.LockFree = %q, want %q", gotLockFree, wantLockFree)
	}
}

// TestRunUsageOnly checks that runs that can only print usage or a parse
// error exit with a usage error before the root check, the admin check and
// the lock, with or without a googet root.
func TestRunUsageOnly(t *testing.T) {
	for _, tc := range [][]string{
		{"install", "-h"},
		{"install", "-help"},
		{"install", "-bogus"},
		{"remove", "-dry_run=maybe", "A"},
		{"nosuchcmd"},
		{"REMOVE", "-dry_run", "A"},
	} {
		t.Run(strings.Join(tc, " "), func(t *testing.T) {
			check := func(t *testing.T, res runResult) {
				t.Helper()
				if res.code != int(subcommands.ExitUsageError) {
					t.Errorf("googet %q exit code = %d, want %d; output:\n%s", tc, res.code, subcommands.ExitUsageError, res.output())
				}
				if len(res.spies) != 0 {
					t.Errorf("googet %q called spies %v, want none", tc, res.spies)
				}
				if res.stderr == "" {
					t.Errorf("googet %q printed nothing on stderr, want usage or a parse error", tc)
				}
			}
			t.Run("no root", func(t *testing.T) {
				check(t, runGooget(t, false, tc...))
			})
			t.Run("missing root", func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "googet")
				check(t, runGooget(t, false, append([]string{"-root", root}, tc...)...))
				if _, err := os.Lstat(root); !os.IsNotExist(err) {
					t.Errorf("googet %q created the root %q (Lstat error: %v)", tc, root, err)
				}
			})
			t.Run("existing root", func(t *testing.T) {
				root := fixtureRoot(t)
				before := snapshotRoot(t, root)
				check(t, runGooget(t, false, append([]string{"-root", root}, tc...)...))
				after := snapshotRoot(t, root)
				if !slices.Equal(after.names, before.names) {
					t.Errorf("root files changed from %q to %q", before.names, after.names)
				}
				if !bytes.Equal(after.db, before.db) || !after.dbMtime.Equal(before.dbMtime) {
					t.Error("googet.db changed, want its bytes and mtime unchanged")
				}
			})
		})
	}
}

// TestRunRemoveDryRunUnprivileged checks that remove -dry_run runs without
// the lock when the admin check fails, and leaves the googet root untouched.
func TestRunRemoveDryRunUnprivileged(t *testing.T) {
	root := fixtureRoot(t)
	before := snapshotRoot(t, root)

	res := runGooget(t, false, "-root", root, "remove", "-dry_run", "A")

	if res.code != 0 {
		t.Errorf("googet remove -dry_run A exited %d, want 0; stderr:\n%s", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "B.noarch") || !strings.Contains(res.stdout, "Dry run") {
		t.Errorf("googet remove -dry_run A printed %q, want the dry-run list naming B", res.stdout)
	}
	if !res.spies["isAdmin"] || res.spies["obtainLock"] {
		t.Errorf("spies called: %v, want only isAdmin", res.spies)
	}
	after := snapshotRoot(t, root)
	if !slices.Equal(after.names, before.names) {
		t.Errorf("root files changed from %q to %q; want no log, lock, journal, cache or repo files", before.names, after.names)
	}
	if !bytes.Equal(after.db, before.db) || !after.dbMtime.Equal(before.dbMtime) {
		t.Error("googet.db changed, want its bytes and mtime unchanged")
	}
}

// TestRunRemoveUnprivileged checks that a remove that isn't a dry run still
// goes through the admin check and stops when it fails.
func TestRunRemoveUnprivileged(t *testing.T) {
	root := fixtureRoot(t)

	res := runGooget(t, false, "-root", root, "remove", "A")

	if res.code != 1 {
		t.Errorf("googet remove A exited %d, want 1; stderr:\n%s", res.code, res.stderr)
	}
	if !res.spies["isAdmin"] {
		t.Error("isAdmin was not called, want the admin check")
	}
	if res.spies["obtainLock"] {
		t.Error("obtainLock was called after a failed admin check")
	}
	if !installed(t, root, "A") {
		t.Error("package A was removed, want it still installed")
	}
}

// TestRunRemovePrivileged checks that run() marks the context as privileged
// once the admin check and the lock succeed, so remove still removes.
func TestRunRemovePrivileged(t *testing.T) {
	root := fixtureRoot(t)

	res := runGooget(t, true, "-root", root, "-noconfirm", "-system_log=false", "remove", "-db_only", "B")

	if res.code != 0 {
		t.Errorf("googet remove -db_only B with privileges exited %d, want 0; stderr:\n%s", res.code, res.stderr)
	}
	if !res.spies["isAdmin"] || !res.spies["obtainLock"] {
		t.Errorf("spies called: %v, want isAdmin and obtainLock", res.spies)
	}
	if installed(t, root, "B") {
		t.Error("package B is still installed, want it removed")
	}
}

// TestRunElevatedAllowUnelevated checks elevated runs of commands that may
// run unelevated. A cli.LockFree command still runs without the admin check,
// the lock and googet.log. Any other command takes the privileged path, so
// the admin check passes, it takes the lock and it writes googet.log.
func TestRunElevatedAllowUnelevated(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		wantOut    string
		privileged bool
	}{
		{args: []string{"remove", "-dry_run", "A"}, wantOut: "Dry run", privileged: true},
		{args: []string{"installed"}, wantOut: "A.noarch"},
		{args: []string{"listrepos"}, wantOut: "test"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := fixtureRoot(t)
			if err := os.Mkdir(filepath.Join(root, "repos"), 0774); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "repos", "test.repo"), []byte("- name: test\n  url: https://example.com/repo\n"), 0664); err != nil {
				t.Fatal(err)
			}
			res := runGooget(t, true, append([]string{"-root", root, "-noconfirm", "-system_log=false"}, tc.args...)...)
			if res.code != 0 {
				t.Errorf("elevated googet %q exit code = %d, want 0; output:\n%s", tc.args, res.code, res.output())
			}
			if !strings.Contains(res.stdout, tc.wantOut) {
				t.Errorf("elevated googet %q printed %q, want it to contain %q", tc.args, res.stdout, tc.wantOut)
			}
			wantSpies := map[string]bool{}
			if tc.privileged {
				wantSpies = map[string]bool{"isAdmin": true, "obtainLock": true}
			}
			if !maps.Equal(res.spies, wantSpies) {
				t.Errorf("elevated googet %q called spies %v, want %v", tc.args, res.spies, wantSpies)
			}
			_, err := os.Stat(filepath.Join(root, "googet.log"))
			if gotLog := err == nil; gotLog != tc.privileged {
				t.Errorf("elevated googet %q wrote googet.log: %v (Stat error: %v), want %v", tc.args, gotLog, err, tc.privileged)
			}
		})
	}
}

func TestRunUnelevatedCreatesNoRoot(t *testing.T) {
	for _, tc := range []struct {
		desc string
		args []string
	}{
		{desc: "listrepos", args: []string{"listrepos"}},
		{desc: "installed", args: []string{"installed"}},
		{desc: "remove dry run", args: []string{"remove", "-dry_run", "foo"}},
		{desc: "install", args: []string{"install", "foo"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			args := append([]string{"-root", root, "-system_log=false", "-noconfirm"}, tc.args...)
			res := runGooget(t, false, args...)
			if res.code != 1 {
				t.Errorf("unelevated googet %q exit code = %d, want 1; output:\n%s", tc.args, res.code, res.output())
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("unelevated googet %q left the root %q behind (Lstat error: %v); output:\n%s", tc.args, root, err, res.output())
			}
		})
	}
}

func TestRunUnelevatedWithEmptyRoot(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		args     []string
		wantCode int
	}{
		// Unelevated runs need googet.db, which an unelevated run must not
		// create, so both commands take the privileged path and fail the admin
		// check.
		{desc: "installed", args: []string{"installed"}, wantCode: 1},
		{desc: "listrepos", args: []string{"listrepos"}, wantCode: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			args := append([]string{"-root", root, "-system_log=false"}, tc.args...)
			res := runGooget(t, false, args...)
			if res.code != tc.wantCode {
				t.Errorf("unelevated googet %q with an empty root exit code = %d, want %d; output:\n%s", tc.args, res.code, tc.wantCode, res.output())
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("reading root: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("unelevated googet %q created %v in the root, want nothing", tc.args, entries)
			}
		})
	}
}

func TestRunElevatedCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	res := runGooget(t, true, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
	if res.code != 0 {
		t.Fatalf("elevated googet listrepos exit code = %d, want 0; output:\n%s", res.code, res.output())
	}
	for _, p := range []struct {
		path string
		dir  bool
	}{
		{root, true},
		{filepath.Join(root, "cache"), true},
		{filepath.Join(root, "repos"), true},
		{filepath.Join(root, "googet.db"), false},
	} {
		fi, err := os.Stat(p.path)
		if err != nil {
			t.Errorf("elevated googet listrepos didn't create %q: %v", p.path, err)
			continue
		}
		if fi.IsDir() != p.dir {
			t.Errorf("%q IsDir() = %v, want %v", p.path, fi.IsDir(), p.dir)
		}
	}
}

func TestRunWithoutRoot(t *testing.T) {
	for _, tc := range []struct {
		desc    string
		args    []string
		wantOut string
	}{
		{desc: "version", args: []string{"-version"}, wantOut: "GooGet version:"},
		{desc: "help", args: []string{"help"}, wantOut: "Subcommands"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			for _, args := range [][]string{tc.args, append([]string{"-root", root}, tc.args...)} {
				res := runGooget(t, false, args...)
				if res.code != 0 {
					t.Errorf("googet %q exit code = %d, want 0; output:\n%s", args, res.code, res.output())
				}
				if !strings.Contains(res.output(), tc.wantOut) {
					t.Errorf("googet %q output = %q, want it to contain %q", args, res.output(), tc.wantOut)
				}
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("googet %q created the root %q (Lstat error: %v)", tc.args, root, err)
			}
		})
	}
}
