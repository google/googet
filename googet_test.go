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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"io/ioutil"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/googet/v2/testutil"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

// childEnvPrefix starts the name of every environment variable below.
const childEnvPrefix = "GOOGET_TEST_"

// Environment variables that configure a child process of the tests. Every
// name starts with childEnvPrefix, so that childEnv can drop inherited ones.
const (
	// runArgsEnv, when set, makes the test binary run googet's run() with
	// the JSON-encoded arguments it holds instead of running tests. run()
	// registers flags on flag.CommandLine and commands on
	// subcommands.DefaultCommander, so each run needs a fresh process.
	runArgsEnv = childEnvPrefix + "RUN_ARGS"
	// spyDirEnv names the directory where the isAdmin and obtainLock spies
	// record that they were called.
	spyDirEnv = childEnvPrefix + "SPY_DIR"
	// allowEnv, when set, makes the admin check and the lock succeed.
	// Otherwise the child behaves as an unelevated process.
	allowEnv = childEnvPrefix + "ALLOW_PRIVILEGE"
	// confineEnv, when set, makes the child deny itself every filesystem
	// write outside the directories listed below before it runs googet,
	// where the OS supports that. reexecGuardEnv marks the child after it has
	// confined itself and re-executed.
	confineEnv     = childEnvPrefix + "CONFINE_WRITES"
	reexecGuardEnv = childEnvPrefix + "CONFINE_REEXECED"
	// scratchEnv names the scratch tree that holds an unelevated child's
	// temporary, home and working directories, and rootEnv names the googet
	// root it was given with -root. Writes under both stay allowed (under the
	// root's nearest existing ancestor while the root doesn't exist), so that
	// runGooget's snapshot of the scratch tree and the callers' snapshots of
	// the root show them, even when googet ignores the error that a denied
	// write would return.
	scratchEnv = childEnvPrefix + "SCRATCH"
	rootEnv    = childEnvPrefix + "ROOT"
	// writeProbeEnv, when set, makes the child write an empty file at the
	// path it holds instead of running googet. The child exits 0 if the write
	// works, probeDeniedCode if it fails with a permission error and 1 if it
	// fails otherwise. TestWriteConfinement uses it.
	writeProbeEnv = childEnvPrefix + "WRITE_PROBE"
)

// Exit codes and messages that a child uses to report its result.
const (
	probeDeniedCode = 3
	// confineFailCode and confineFailPrefix mark a child that couldn't
	// confine its writes.
	confineFailCode   = 125
	confineFailPrefix = "confining writes: "
)

// confineFailed reports whether a child exited because it couldn't confine
// its writes.
func confineFailed(code int, stderr string) bool {
	return code == confineFailCode && strings.Contains(stderr, confineFailPrefix)
}

// TestMain serves all tests in package main. When runArgsEnv or
// writeProbeEnv is set, the test binary acts as a child process for
// runGooget or TestWriteConfinement instead of running the tests.
func TestMain(m *testing.M) {
	args, probe := os.Getenv(runArgsEnv), os.Getenv(writeProbeEnv)
	if args == "" && probe == "" {
		os.Exit(m.Run())
	}
	if os.Getenv(confineEnv) != "" && os.Getenv(reexecGuardEnv) == "" {
		allow := []string{os.Getenv(spyDirEnv)}
		if d := os.Getenv(scratchEnv); d != "" {
			allow = append(allow, d)
		}
		if d := os.Getenv(rootEnv); d != "" {
			// The root may not exist yet. Allow its nearest existing
			// ancestor, normally a test's own temporary
			// directory, so that creating the root shows up too.
			for !isDir(d) && filepath.Dir(d) != d {
				d = filepath.Dir(d)
			}
			// Never allow a whole volume.
			if filepath.Dir(d) != d {
				allow = append(allow, d)
			}
		}
		// A binary built with coverage writes its counters to GOCOVERDIR
		// when it exits.
		if d := os.Getenv("GOCOVERDIR"); d != "" {
			allow = append(allow, d)
		}
		err := confineWritesAndExec(allow, append(os.Environ(), reexecGuardEnv+"=1"))
		fmt.Fprintf(os.Stderr, "%s%v\n", confineFailPrefix, err)
		os.Exit(confineFailCode)
	}
	if probe != "" {
		err := os.WriteFile(probe, nil, 0644)
		switch {
		case err == nil:
			os.Exit(0)
		case errors.Is(err, fs.ErrPermission):
			fmt.Fprintf(os.Stderr, "write probe: %v\n", err)
			os.Exit(probeDeniedCode)
		default:
			fmt.Fprintf(os.Stderr, "write probe: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(runWithSpies(args))
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

// childEnv returns this process's environment with the variables in set
// replaced by their values in set. GooGetRoot and every inherited variable
// whose name starts with childEnvPrefix are dropped unless set has them, so
// the parent's environment can't change how a child runs. Names are compared
// without regard to case, as Windows does.
func childEnv(set map[string]string) []string {
	skip := map[string]bool{strings.ToUpper(envVar): true}
	for k := range set {
		skip[strings.ToUpper(k)] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if uk := strings.ToUpper(k); !skip[uk] && !strings.HasPrefix(uk, childEnvPrefix) {
			env = append(env, kv)
		}
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	return env
}

// runGooget runs googet with args in a child process, with isAdmin and
// obtainLock replaced by spies that succeed if allow is set and fail
// otherwise. The GooGetRoot environment variable is unset, so only -root
// sets the root.
//
// An unelevated run (allow unset) must not write any file, inside the googet
// root or anywhere else. Callers check the root. runGooget points the
// child's temporary, home and application data directories and its working
// directory into a scratch tree and fails the test if that tree changes.
// Where the kernel supports Landlock, the child also denies itself every
// filesystem write outside the spy directory, the scratch tree and the root
// (or the root's nearest existing ancestor while the root doesn't exist), so
// that a write anywhere else fails in the child. On Linux without
// Landlock only the snapshots run, and the test fails instead when CI is
// set. A denied write whose error googet ignores goes unreported, and
// Landlock doesn't cover changes to a file's mode, owner, times or extended
// attributes.
func runGooget(t *testing.T, allow bool, args ...string) runResult {
	t.Helper()
	enc, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	spyDir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	env := map[string]string{
		runArgsEnv: string(enc),
		spyDirEnv:  spyDir,
		// The race detector otherwise sleeps for a second before each exit.
		"GORACE": strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0"),
	}
	var scratch string
	if allow {
		env[allowEnv] = "1"
	} else {
		scratch = t.TempDir()
		env[scratchEnv] = scratch
		tmp, home, cwd := filepath.Join(scratch, "tmp"), filepath.Join(scratch, "home"), filepath.Join(scratch, "cwd")
		for _, d := range []string{tmp, home, cwd} {
			if err := os.Mkdir(d, 0755); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
			env[k] = tmp
		}
		for _, k := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
			env[k] = home
		}
		cmd.Dir = cwd
		if root, ok := rootArg(args); ok {
			env[rootEnv] = root
		}
		if writeConfinementSupported() {
			env[confineEnv] = "1"
		} else if runtime.GOOS == "linux" && os.Getenv("CI") != "" {
			t.Fatal("Landlock is not available, so unelevated runs can't be checked for writes outside the googet root")
		}
	}
	cmd.Env = childEnv(env)
	var before map[string]string
	if scratch != "" {
		before = testutil.TreeSnapshot(t, scratch)
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
	if confineFailed(res.code, res.stderr) {
		t.Fatalf("googet %q: %s", args, res.stderr)
	}
	if scratch != "" {
		if diff := cmp.Diff(before, testutil.TreeSnapshot(t, scratch)); diff != "" {
			t.Errorf("unelevated googet %q wrote outside the googet root (-before +after):\n%s", args, diff)
		}
	}
	for _, s := range []string{"isAdmin", "obtainLock"} {
		if _, err := os.Stat(filepath.Join(spyDir, s)); err == nil {
			res.spies[s] = true
		}
	}
	return res
}

// rootArg returns the value of the global -root flag in args, the last one
// if it is repeated, as the flag package keeps. It handles the forms these
// tests use: one or two leading dashes, the value either after '=' or in the
// next argument, and it stops at "--" or at the first argument that isn't a
// flag. Unlike the flag package, it skips malformed flags such as "---root"
// instead of rejecting them.
func rootArg(args []string) (root string, ok bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(a[1:], "-"), "=")
		if name != "root" {
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			val = args[i]
		}
		root, ok = val, true
	}
	return root, ok
}

func TestRootArg(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		want   string
		wantOK bool
	}{
		{[]string{"-root", "r", "installed"}, "r", true},
		{[]string{"--root", "r", "installed"}, "r", true},
		{[]string{"-root=r", "installed"}, "r", true},
		{[]string{"--root=r", "installed"}, "r", true},
		{[]string{"-noconfirm", "-root", "r", "installed"}, "r", true},
		{[]string{"-root", "a", "--root=b", "installed"}, "b", true},
		{[]string{"--root=a", "-root", "b", "installed"}, "b", true},
		{[]string{"-root"}, "", false},
		{[]string{"installed", "-root", "r"}, "", false},
		{[]string{"install", "root"}, "", false},
		{[]string{"install", "root=r"}, "", false},
		{[]string{"--", "-root", "r"}, "", false},
		{[]string{"---root", "r"}, "", false},
	} {
		if got, ok := rootArg(tc.args); got != tc.want || ok != tc.wantOK {
			t.Errorf("rootArg(%q) = %q, %v, want %q, %v", tc.args, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestWriteConfinement checks that the write confinement runGooget uses for
// unelevated runs allows writes only in the spy directory and under the root,
// or under the root's nearest existing ancestor while the root doesn't exist,
// and never under a whole volume.
func TestWriteConfinement(t *testing.T) {
	if !writeConfinementSupported() {
		t.Skip("write confinement is not supported on this system")
	}
	spyDir := t.TempDir()
	existingRoot := t.TempDir()
	missingRootParent := t.TempDir()
	missingRoot := filepath.Join(missingRootParent, "googet")
	volumeChildRoot := filepath.Join(string(filepath.Separator), "nonexistent-googet-test", "googet")
	for _, tc := range []struct {
		name      string
		path      string
		root      string
		wantWrite bool
	}{
		{"outside the spy directory", filepath.Join(t.TempDir(), "probe"), "", false},
		{"in the spy directory", filepath.Join(spyDir, "probe"), "", true},
		{"under an existing root", filepath.Join(existingRoot, "probe"), existingRoot, true},
		{"beside an existing root", filepath.Join(filepath.Dir(existingRoot), "probe-beside-root"), existingRoot, false},
		{"beside a missing root", filepath.Join(missingRootParent, "probe"), missingRoot, true},
		{"outside a missing root's parent", filepath.Join(t.TempDir(), "probe"), missingRoot, false},
		{"missing root whose only existing ancestor is the volume", filepath.Join(t.TempDir(), "probe"), volumeChildRoot, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := map[string]string{spyDirEnv: spyDir, confineEnv: "1", writeProbeEnv: tc.path}
			if tc.root != "" {
				set[rootEnv] = tc.root
			}
			cmd := exec.Command(os.Args[0])
			cmd.Env = childEnv(set)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			code := 0
			var exitErr *exec.ExitError
			if err := cmd.Run(); errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("running the write probe: %v", err)
			}
			if confineFailed(code, stderr.String()) {
				t.Fatalf("child failed to confine writes: %s", stderr.String())
			}
			wantCode := probeDeniedCode
			if tc.wantWrite {
				wantCode = 0
			}
			if code != wantCode {
				t.Errorf("write probe of %q exit code = %d, want %d (0: written, %d: permission denied); stderr: %s", tc.path, code, wantCode, probeDeniedCode, stderr.String())
			}
			_, statErr := os.Stat(tc.path)
			if wrote := statErr == nil; wrote != tc.wantWrite {
				t.Errorf("probe file %q exists: %v (Stat error %v), want %v", tc.path, wrote, statErr, tc.wantWrite)
			}
		})
	}
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
		{"available", cli.UnelevatedRun},
		{"available -info pkg", cli.UnelevatedRun},
		{"latest pkg", cli.UnelevatedRun},
		{"latest -compare -json pkg", cli.UnelevatedRun},
		{"update -dry_run", cli.UnelevatedRun},
		{"update", cli.Privileged},
		{"install -dry_run pkg", cli.UnelevatedRun},
		{"install -dry_run -db_only pkg", cli.UnelevatedRun},
		{"install pkg", cli.Privileged},
		{"install -dry_run -reinstall pkg", cli.Privileged},
		{"install -reinstall -dry_run pkg", cli.Privileged},
		{"install -dry_run -redownload pkg", cli.Privileged},
		{"install -dry_run -reinstall=false pkg", cli.UnelevatedRun},
		{"download pkg", cli.Privileged},
		{"download -download_dir x pkg", cli.Privileged},
		{"check", cli.Privileged},
		{"check -dry_run", cli.Privileged},
		{"verify pkg", cli.Privileged},
		{"verify -skip_files pkg", cli.Privileged},
		{"clean", cli.Privileged},
		{"clean -all", cli.Privileged},
		{"addrepo name https://example.com/repo", cli.Privileged},
		{"rmrepo name", cli.Privileged},
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
	wantAllow := []string{"available", "install", "installed", "latest", "listrepos", "remove", "update"}
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

// logCapture collects what the logger writes, for in-process tests.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// take returns what was logged since the last call and clears it.
func (c *logCapture) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.buf.String()
	c.buf.Reset()
	return s
}

var (
	capturedLogsOnce sync.Once
	capturedLogs     = &logCapture{}
)

// captureLogs makes the default logger write to capturedLogs. The logger
// keeps the first Init, so this works only if nothing in this test process
// initialized it before.
func captureLogs() *logCapture {
	capturedLogsOnce.Do(func() { logger.Init("GooGet", false, false, capturedLogs) })
	capturedLogs.take()
	return capturedLogs
}

// registeredCommand returns the command registered under name, as
// subcommands.Commander.Execute finds it.
func registeredCommand(t *testing.T, name string) subcommands.Command {
	t.Helper()
	var cmd subcommands.Command
	subcommands.DefaultCommander.VisitCommands(func(_ *subcommands.CommandGroup, c subcommands.Command) {
		if cmd == nil && c.Name() == name {
			cmd = c
		}
	})
	if cmd == nil {
		t.Fatalf("command %q isn't registered", name)
	}
	return cmd
}

// captureOutput runs f with os.Stdout and os.Stderr redirected to a
// temporary file, and returns what f wrote to them.
func captureOutput(t *testing.T, f func()) string {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = tmp, tmp
	f()
	os.Stdout, os.Stderr = oldOut, oldErr
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestExecuteRefusesUnelevatedFirst runs registered commands' Execute
// without the privileged marker, with flags that their AllowUnelevated
// doesn't allow or, for a command that doesn't implement it, any flags,
// against a root that has no database, cache or repos. Each must print
// exactly one line, its refusal, before it opens the database, builds
// sources or fetches anything, and create nothing.
func TestExecuteRefusesUnelevatedFirst(t *testing.T) {
	logs := captureLogs()
	for _, args := range [][]string{
		{"install", "-dry_run", "-reinstall", "pkg"},
		{"install", "-dry_run", "-redownload", "-reinstall", "pkg"},
		{"install", "pkg"},
		{"update"},
		{"update", "-dry_run=false"},
		{"download", "pkg"},
		{"remove", "pkg"},
		{"check"},
		{"clean", "-all"},
		{"verify", "pkg"},
		{"addrepo", "name", "https://example.com/repo"},
		{"rmrepo", "name"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := t.TempDir()
			settings.Initialize(root, false)
			cmd := registeredCommand(t, args[0])
			fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
			cmd.SetFlags(fs)
			if err := fs.Parse(args[1:]); err != nil {
				t.Fatalf("parsing %q: %v", args, err)
			}
			var got subcommands.ExitStatus
			printed := captureOutput(t, func() { got = cmd.Execute(context.Background(), fs) })
			if got != subcommands.ExitFailure {
				t.Errorf("unprivileged Execute(%q) = %v, want %v", args, got, subcommands.ExitFailure)
			}
			// The logger writes to logs and to the stderr it saw at Init,
			// which captureOutput doesn't capture, so nothing is counted
			// twice.
			out := strings.TrimSpace(logs.take() + printed)
			if lines := strings.Split(out, "\n"); len(lines) != 1 || !strings.Contains(out, "requires administrator privileges") {
				t.Errorf("unprivileged Execute(%q) printed %q, want exactly one line, the refusal", args, out)
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Errorf("unprivileged Execute(%q) left %v in the root (error %v), want nothing", args, entries, err)
			}
		})
	}
}

// readOnlyRoot returns fixtureRoot's root with a googet.conf that allows HTTP
// repos, a repos folder naming a repo served by a test server, and a stale
// cache entry for that repo. The repo
// has A 2.0.0@1, an update for the installed A, and C 1.0.0@1. Everything
// under the root is backdated, so any write moves an mtime.
func readOnlyRoot(t *testing.T) string {
	t.Helper()
	specs := []goolib.RepoSpec{
		{Source: "A.noarch.2.0.0@1.goo", PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "2.0.0@1"}},
		{Source: "C.noarch.1.0.0@1.goo", PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "1.0.0@1"}},
	}
	j, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(j)
	}))
	t.Cleanup(srv.Close)

	root := fixtureRoot(t)
	for _, d := range []string{"repos", "cache"} {
		if err := os.Mkdir(filepath.Join(root, d), 0774); err != nil {
			t.Fatal(err)
		}
	}
	cacheBase := filepath.Join("cache", fmt.Sprintf("%x", sha256.Sum256([]byte(srv.URL))))
	files := map[string]string{
		"googet.conf":                       "allowunsafeurl: true\n",
		filepath.Join("repos", "test.repo"): fmt.Sprintf("- name: test\n  url: %s\n", srv.URL),
		cacheBase + ".rs":                   "[]",
		cacheBase + ".url":                  srv.URL,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0664); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range append(slices.Collect(maps.Keys(files)), "repos", "cache", ".") {
		if err := os.Chtimes(filepath.Join(root, p), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestRunUnelevatedReadsLeaveRootUnchanged runs the read-only commands
// unelevated, with failing admin and lock spies, against a root with a stale
// repo index cache. Each must succeed without taking the lock, write no
// googet.log and leave every name, byte and mtime under the root unchanged.
// Only the cli.LockFree commands skip the admin check too.
func TestRunUnelevatedReadsLeaveRootUnchanged(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantOut  string
		lockFree bool
	}{
		{args: []string{"available"}, wantOut: "C.noarch"},
		{args: []string{"latest", "A"}, wantOut: "2.0.0@1"},
		{args: []string{"latest", "-compare", "A"}, wantOut: "2.0.0@1"},
		{args: []string{"update", "-dry_run"}, wantOut: "Dry run"},
		{args: []string{"install", "-dry_run", "C"}, wantOut: "C.noarch"},
		{args: []string{"installed"}, wantOut: "A.noarch", lockFree: true},
		{args: []string{"listrepos"}, wantOut: "test", lockFree: true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := readOnlyRoot(t)
			before := testutil.TreeSnapshot(t, root)
			res := runGooget(t, false, append([]string{"-root", root, "-noconfirm"}, tc.args...)...)
			if res.code != 0 {
				t.Errorf("unelevated googet %q exit code = %d, want 0; output:\n%s", tc.args, res.code, res.output())
			}
			if !strings.Contains(res.stdout, tc.wantOut) {
				t.Errorf("unelevated googet %q printed %q, want it to contain %q", tc.args, res.stdout, tc.wantOut)
			}
			wantAdminCheck := !tc.lockFree
			if res.spies["isAdmin"] != wantAdminCheck || res.spies["obtainLock"] {
				t.Errorf("unelevated googet %q called spies %v, want isAdmin: %v and no lock", tc.args, res.spies, wantAdminCheck)
			}
			if diff := cmp.Diff(before, testutil.TreeSnapshot(t, root)); diff != "" {
				t.Errorf("unelevated googet %q changed the root (-before +after):\n%s", tc.args, diff)
			}
		})
	}
}

// TestRunUnelevatedWritesStayPrivileged checks that commands and flags that
// write or run package code still go through the admin check unelevated, and
// that the failed check leaves the root unchanged.
func TestRunUnelevatedWritesStayPrivileged(t *testing.T) {
	for _, args := range [][]string{
		{"update"},
		{"install", "C"},
		{"install", "-dry_run", "-reinstall", "A"},
		{"install", "-dry_run", "-redownload", "-reinstall", "A"},
		{"download", "C"},
		{"check"},
		{"check", "-dry_run"},
		{"verify", "A"},
		{"clean"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := readOnlyRoot(t)
			before := testutil.TreeSnapshot(t, root)
			res := runGooget(t, false, append([]string{"-root", root, "-noconfirm"}, args...)...)
			if res.code != 1 {
				t.Errorf("unelevated googet %q exit code = %d, want 1; output:\n%s", args, res.code, res.output())
			}
			if !res.spies["isAdmin"] || res.spies["obtainLock"] {
				t.Errorf("unelevated googet %q called spies %v, want only isAdmin", args, res.spies)
			}
			if diff := cmp.Diff(before, testutil.TreeSnapshot(t, root)); diff != "" {
				t.Errorf("unelevated googet %q changed the root (-before +after):\n%s", args, diff)
			}
		})
	}
}

// TestRunElevatedAllowUnelevated checks elevated runs of commands that may
// run unelevated, against a root with a stale repo index cache. A
// cli.LockFree command still runs without the admin check, the lock and
// googet.log. Any other command takes the privileged path: the admin check
// passes, it takes the lock, writes googet.log and, if it reads the index,
// refreshes the stale cache.
func TestRunElevatedAllowUnelevated(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		wantOut     string
		privileged  bool
		wantRefresh bool
	}{
		{args: []string{"remove", "-dry_run", "A"}, wantOut: "Dry run", privileged: true},
		{args: []string{"latest", "A"}, wantOut: "2.0.0@1", privileged: true, wantRefresh: true},
		{args: []string{"update", "-dry_run"}, wantOut: "Dry run", privileged: true, wantRefresh: true},
		{args: []string{"installed"}, wantOut: "A.noarch"},
		{args: []string{"listrepos"}, wantOut: "test"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := readOnlyRoot(t)
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
			caches, err := filepath.Glob(filepath.Join(root, "cache", "*.rs"))
			if err != nil || len(caches) != 1 {
				t.Fatalf("cache files = %q (error %v), want one", caches, err)
			}
			b, err := os.ReadFile(caches[0])
			if err != nil {
				t.Fatal(err)
			}
			if refreshed := string(b) != "[]"; refreshed != tc.wantRefresh {
				t.Errorf("elevated googet %q left the cache holding %q; refreshed: %v, want %v", tc.args, b, refreshed, tc.wantRefresh)
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
