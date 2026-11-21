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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
)

const (
	// runArgsEnv, when set, makes the test binary run googet's run() with
	// the JSON-encoded arguments it holds instead of running tests. run()
	// registers flags on the global FlagSet, so each call needs a fresh
	// process.
	runArgsEnv = "GOOGET_TEST_RUN_ARGS"
	// spyDirEnv names the directory where the isAdmin and obtainLock spies
	// record that they were called.
	spyDirEnv = "GOOGET_TEST_SPY_DIR"
	// allowEnv, when set, makes the spies succeed instead of failing.
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

// runWithSpies runs googet with isAdmin and obtainLock replaced by spies
// that record the call and fail, or succeed if allowEnv is set.
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
	obtainLock = func(string, time.Duration) (func(), error) {
		record("obtainLock")
		if allow {
			return func() {}, nil
		}
		return nil, errors.New("spy: lock refused")
	}
	os.Args = append([]string{"googet"}, args...)
	return run(context.Background())
}

type runResult struct {
	code           int
	stdout, stderr string
	spies          map[string]bool // Spies that were called.
}

// runGooget runs googet with args in a subprocess, with isAdmin and
// obtainLock replaced by spies that succeed if allow is set and fail
// otherwise.
func runGooget(t *testing.T, allow bool, args ...string) runResult {
	t.Helper()
	enc, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	spyDir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runArgsEnv+"="+string(enc), spyDirEnv+"="+spyDir, envVar+"=")
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

// TestNeedsPrivilege checks the command name and database conditions. The
// remove flag combinations are covered by remove.TestDryRunRequested.
func TestNeedsPrivilege(t *testing.T) {
	for _, tc := range []struct {
		cmd      string
		args     string
		dbExists bool
		want     bool
	}{
		{"remove", "-dry_run pkg", true, false},
		{"remove", "pkg", true, true},
		{"remove", "-dry_run pkg", false, true},
		{"install", "-dry_run pkg", true, true},
	} {
		t.Run(fmt.Sprintf("%s %s dbExists=%v", tc.cmd, tc.args, tc.dbExists), func(t *testing.T) {
			if got := needsPrivilege(tc.cmd, strings.Fields(tc.args), tc.dbExists); got != tc.want {
				t.Errorf("needsPrivilege(%q, %q, %v) = %v, want %v", tc.cmd, tc.args, tc.dbExists, got, tc.want)
			}
		})
	}
}

// TestRunRemoveDryRunUnprivileged checks that remove -dry_run runs without
// the admin check or the lock and leaves the googet root untouched.
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
	if len(res.spies) != 0 {
		t.Errorf("spies called: %v, want none", res.spies)
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
