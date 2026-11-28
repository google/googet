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
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
)

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

// Environment variables that runGooget sets for the child process.
const (
	// runChildEnv, when set to "1", makes the test binary run googet's run()
	// with its arguments instead of the tests. run() registers flags on
	// flag.CommandLine and commands on subcommands.DefaultCommander, so each
	// run needs a fresh process.
	runChildEnv = "GOOGET_TEST_RUN_CHILD"
	// childAdminEnv, when set to "1", makes the child's admin check pass.
	// Otherwise the child behaves as an unelevated process.
	childAdminEnv = "GOOGET_TEST_CHILD_ADMIN"
	// childTraceEnv is a file path where the child appends one line per step
	// of run() that it calls.
	childTraceEnv = "GOOGET_TEST_CHILD_TRACE"
	// childPrepareModeEnv overrides prepareRoot in the child process:
	// "fail" returns an error, "repaired" creates the root and returns a
	// repair report with a warning, "repair_conf" replaces an unreadable
	// googet.conf entry with a valid file, and "created" creates the root and
	// returns Created only.
	childPrepareModeEnv = "GOOGET_TEST_CHILD_PREPARE_MODE"
	// childCheckModeEnv overrides checkEntries in the child process:
	// "lock-fail" reports a quarantined googet.conf on the pre-lock check,
	// "lock-err" returns an error on the pre-lock check, "post-fail" reports
	// an untrusted googet.db on the post-lock check, and "post-err" returns
	// an error on the post-lock check.
	childCheckModeEnv = "GOOGET_TEST_CHILD_CHECK_MODE"
	// childRecordLockAgeEnv, when set to "1", makes obtainLock print the
	// maxAge argument prefixed by lockAgePrefix.
	childRecordLockAgeEnv = "GOOGET_TEST_CHILD_RECORD_LOCK_AGE"
	lockAgePrefix         = "googet-test-lock-age: "
)

func TestMain(m *testing.M) {
	if os.Getenv(runChildEnv) == "1" {
		os.Exit(runChild())
	}
	os.Exit(m.Run())
}

func appendTrace(line string) {
	p := os.Getenv(childTraceEnv)
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// rulePhase identifies which checkEntries rule set rules belongs to.
func rulePhase(rules []system.EntryRule) string {
	for _, r := range rules {
		switch r.Pattern {
		case "googet.lock":
			return "pre-lock"
		case "googet.db":
			return "post-lock"
		}
	}
	return "unknown"
}

// runChild runs run() in a child process started by runGooget. isAdmin fails
// unless the parent allowed admin rights. obtainLock fails if the googet root
// doesn't exist yet, because the root must be created, with its own mode,
// before the lock file.
func runChild() int {
	if os.Getenv(childAdminEnv) == "1" {
		isAdmin = func() error {
			appendTrace("isAdmin")
			return nil
		}
	} else {
		isAdmin = func() error {
			appendTrace("isAdmin")
			return errors.New("test process is not elevated")
		}
	}
	prepareRoot = func(root string, subdirs []string) (system.RootReport, error) {
		appendTrace("prepareRoot:" + strings.Join(subdirs, ","))
		switch os.Getenv(childPrepareModeEnv) {
		case "fail":
			return system.RootReport{}, errors.New("injected prepareRoot failure")
		case "repaired":
			if err := os.MkdirAll(root, 0774); err != nil {
				return system.RootReport{}, err
			}
			return system.RootReport{
				DACLReplaced: true,
				Warnings:     []string{"parent directory warning"},
			}, nil
		case "repair_conf":
			if err := os.MkdirAll(root, 0774); err != nil {
				return system.RootReport{}, err
			}
			confPath := filepath.Join(root, "googet.conf")
			if err := os.Remove(confPath); err != nil && !os.IsNotExist(err) {
				return system.RootReport{}, err
			}
			if err := os.WriteFile(confPath, []byte("progress: false\n"), 0644); err != nil {
				return system.RootReport{}, err
			}
			return system.RootReport{DACLReplaced: true}, nil
		case "created":
			if err := os.MkdirAll(root, 0774); err != nil {
				return system.RootReport{}, err
			}
			return system.RootReport{Created: true}, nil
		default:
			return system.PrepareRoot(root, subdirs)
		}
	}
	origCheckEntries := checkEntries
	checkEntries = func(root string, rules []system.EntryRule, prep system.RootReport) (system.EntryReport, error) {
		phase := rulePhase(rules)
		appendTrace("checkEntries:" + phase)
		switch os.Getenv(childCheckModeEnv) {
		case "lock-fail":
			if phase == "pre-lock" {
				conf := filepath.Join(root, "googet.conf")
				return system.EntryReport{
					Quarantined: map[string]string{conf: conf + ".untrusted-1"},
					Failures:    []string{conf + " was renamed to " + conf + ".untrusted-1"},
				}, nil
			}
		case "lock-err":
			if phase == "pre-lock" {
				return system.EntryReport{}, errors.New("injected pre-lock checkEntries error")
			}
		case "post-fail":
			if phase == "post-lock" {
				db := filepath.Join(root, "googet.db")
				return system.EntryReport{
					Failures: []string{db + " has an untrusted owner"},
				}, nil
			}
		case "post-err":
			if phase == "post-lock" {
				return system.EntryReport{}, errors.New("injected post-lock checkEntries error")
			}
		}
		return origCheckEntries(root, rules, prep)
	}
	origLoadConf := loadConf
	loadConf = func() {
		appendTrace("loadConf")
		origLoadConf()
	}
	obtainLock = func(lockFile string, maxAge time.Duration) (func(), error) {
		appendTrace("obtainLock")
		if os.Getenv(childRecordLockAgeEnv) == "1" {
			fmt.Println(lockAgePrefix + maxAge.String())
		}
		root := filepath.Dir(lockFile)
		if !isDir(root) {
			return nil, fmt.Errorf("googet root %q doesn't exist when taking the lock", root)
		}
		return system.ObtainLock(lockFile, maxAge)
	}
	origRotateLogFile := rotateLogFile
	rotateLogFile = func(logPath string, ls int64) error {
		appendTrace("rotateLog")
		return origRotateLogFile(logPath, ls)
	}
	origCreateDB := createDB
	createDB = func(dbFile string) error {
		appendTrace("createDB")
		return origCreateDB(dbFile)
	}
	origMkdirAll := mkdirAll
	mkdirAll = func(path string, perm os.FileMode) error {
		appendTrace("mkdirAll:" + filepath.Base(path))
		return origMkdirAll(path, perm)
	}
	origExecuteCommand := executeCommand
	executeCommand = func(ctx context.Context) int {
		appendTrace("execute")
		return origExecuteCommand(ctx)
	}
	return run(context.Background())
}

// runGooget runs googet with args in a child process and returns its exit
// code and combined output. The GooGetRoot environment variable is unset, so
// only -root sets the root.
func runGooget(t *testing.T, admin bool, args ...string) (int, string) {
	t.Helper()
	return runGoogetEnv(t, admin, nil, args...)
}

// runGoogetEnv is runGooget with extra environment variables for the child.
func runGoogetEnv(t *testing.T, admin bool, extraEnv []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(envVar)+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// The race detector otherwise sleeps for a second before each exit.
	cmd.Env = append(cmd.Env, runChildEnv+"=1", "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	if admin {
		cmd.Env = append(cmd.Env, childAdminEnv+"=1")
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), string(out)
	default:
		t.Fatalf("running googet %q: %v", args, err)
		return 0, ""
	}
}

func readTrace(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading trace file %q: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
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
			code, out := runGooget(t, false, args...)
			if code != 1 {
				t.Errorf("unelevated googet %q exit code = %d, want 1; output:\n%s", tc.args, code, out)
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("unelevated googet %q left the root %q behind (Lstat error: %v); output:\n%s", tc.args, root, err, out)
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
		// installed needs googet.db, which an unelevated run must not create.
		{desc: "installed", args: []string{"installed"}, wantCode: 1},
		{desc: "listrepos", args: []string{"listrepos"}, wantCode: 0},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			args := append([]string{"-root", root, "-system_log=false"}, tc.args...)
			code, out := runGooget(t, false, args...)
			if code != tc.wantCode {
				t.Errorf("unelevated googet %q with an empty root exit code = %d, want %d; output:\n%s", tc.args, code, tc.wantCode, out)
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

// skipUnlessElevatedOnWindows skips the test on Windows unless the process
// is elevated, because the root is created with an owner and ACL that need
// an elevated process. GitHub Actions runners are elevated, so there the test
// runs and fails if it isn't.
func skipUnlessElevatedOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" && system.IsAdmin() != nil && os.Getenv("GITHUB_ACTIONS") == "" {
		t.Skip("needs an elevated process on Windows")
	}
}

func TestRunElevatedCreatesRoot(t *testing.T) {
	skipUnlessElevatedOnWindows(t)
	root := filepath.Join(t.TempDir(), "googet")
	code, out := runGooget(t, true, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
	if code != 0 {
		t.Fatalf("elevated googet listrepos exit code = %d, want 0; output:\n%s", code, out)
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
				code, out := runGooget(t, false, args...)
				if code != 0 {
					t.Errorf("googet %q exit code = %d, want 0; output:\n%s", args, code, out)
				}
				if !strings.Contains(out, tc.wantOut) {
					t.Errorf("googet %q output = %q, want it to contain %q", args, out, tc.wantOut)
				}
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("googet %q created the root %q (Lstat error: %v)", tc.args, root, err)
			}
		})
	}
}

func TestRunPrepareRootOrder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	trace := filepath.Join(t.TempDir(), "trace.txt")
	code, out := runGoogetEnv(t, true, []string{
		childTraceEnv + "=" + trace,
		childPrepareModeEnv + "=created",
	}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
	if code != 0 {
		t.Fatalf("elevated googet listrepos exit code = %d, want 0; output:\n%s", code, out)
	}
	want := []string{
		"isAdmin",
		"prepareRoot:repos,cache",
		"checkEntries:pre-lock",
		"loadConf",
		"obtainLock",
		"checkEntries:post-lock",
		"rotateLog",
		"createDB",
		"mkdirAll:cache",
		"mkdirAll:repos",
		"execute",
	}
	if diff := cmp.Diff(want, readTrace(t, trace)); diff != "" {
		t.Errorf("call order mismatch (-want +got):\n%s", diff)
	}
}

func TestRunPrepareRootFailureStopsBeforeLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	trace := filepath.Join(t.TempDir(), "trace.txt")
	code, out := runGoogetEnv(t, true, []string{
		childTraceEnv + "=" + trace,
		childPrepareModeEnv + "=fail",
	}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "injected prepareRoot failure") {
		t.Errorf("output = %q, want it to contain the prepareRoot error", out)
	}
	want := []string{"isAdmin", "prepareRoot:repos,cache"}
	if diff := cmp.Diff(want, readTrace(t, trace)); diff != "" {
		t.Errorf("call order mismatch (-want +got):\n%s", diff)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("root %q exists after prepareRoot failure (Lstat error: %v)", root, err)
	}
}

func TestRunUnelevatedSkipsPrepareRoot(t *testing.T) {
	t.Run("listrepos with existing root", func(t *testing.T) {
		root := t.TempDir()
		trace := filepath.Join(t.TempDir(), "trace.txt")
		code, out := runGoogetEnv(t, false, []string{
			childTraceEnv + "=" + trace,
		}, "-root", root, "-system_log=false", "listrepos")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		if diff := cmp.Diff([]string{"loadConf", "execute"}, readTrace(t, trace)); diff != "" {
			t.Errorf("trace mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("installed with existing root and db", func(t *testing.T) {
		root := t.TempDir()
		db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
		if err != nil {
			t.Fatalf("creating test db: %v", err)
		}
		db.Close()
		trace := filepath.Join(t.TempDir(), "trace.txt")
		code, out := runGoogetEnv(t, false, []string{
			childTraceEnv + "=" + trace,
		}, "-root", root, "-system_log=false", "installed", "-format=json")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		if diff := cmp.Diff([]string{"loadConf", "execute"}, readTrace(t, trace)); diff != "" {
			t.Errorf("trace mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("install with missing root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		trace := filepath.Join(t.TempDir(), "trace.txt")
		code, out := runGoogetEnv(t, false, []string{
			childTraceEnv + "=" + trace,
		}, "-root", root, "-system_log=false", "install", "foo")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1; output:\n%s", code, out)
		}
		if diff := cmp.Diff([]string{"isAdmin"}, readTrace(t, trace)); diff != "" {
			t.Errorf("trace mismatch (-want +got):\n%s", diff)
		}
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Errorf("missing root %q was created (Lstat error: %v)", root, err)
		}
	})
}

func TestRunLogsPrepareRootReportToLogFile(t *testing.T) {
	t.Run("repaired root and warning logged to googet.log", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		code, out := runGoogetEnv(t, true, []string{
			childPrepareModeEnv + "=repaired",
		}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		logBytes, err := os.ReadFile(filepath.Join(root, "googet.log"))
		if err != nil {
			t.Fatalf("reading googet.log: %v", err)
		}
		logText := string(logBytes)
		if !strings.Contains(logText, "Reset the permissions of the googet root") || !strings.Contains(logText, "replaced the access list") {
			t.Errorf("googet.log missing repair summary; got:\n%s", logText)
		}
		if !strings.Contains(logText, "parent directory warning") {
			t.Errorf("googet.log missing parent warning; got:\n%s", logText)
		}
	})

	t.Run("created root logs no repair warning", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		code, out := runGoogetEnv(t, true, []string{
			childPrepareModeEnv + "=created",
		}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		logBytes, err := os.ReadFile(filepath.Join(root, "googet.log"))
		if err != nil {
			t.Fatalf("reading googet.log: %v", err)
		}
		if strings.Contains(string(logBytes), "Reset the permissions of the googet root") {
			t.Errorf("googet.log unexpectedly logged a repair warning for a newly created root:\n%s", logBytes)
		}
	})
}

func TestRootSubdirsMatchSettings(t *testing.T) {
	orig := settings.RootDir
	t.Cleanup(func() { settings.RootDir = orig })
	settings.RootDir = filepath.Join(t.TempDir(), "googet")
	want := []string{filepath.Base(settings.RepoDir()), filepath.Base(settings.CacheDir())}
	if diff := cmp.Diff(want, rootSubdirs()); diff != "" {
		t.Errorf("rootSubdirs() mismatch (-want +got):\n%s", diff)
	}
}

func TestRunPrepareRootBeforeSettingsInit(t *testing.T) {
	t.Run("prepareRoot failure skips googet.conf", func(t *testing.T) {
		root := t.TempDir()
		// A directory at googet.conf makes ReadFile fail with a non-NotExist
		// error, which panics in settings.readConf if Initialize runs before
		// prepareRoot.
		if err := os.Mkdir(filepath.Join(root, "googet.conf"), 0755); err != nil {
			t.Fatal(err)
		}
		code, out := runGoogetEnv(t, true, []string{
			childPrepareModeEnv + "=fail",
		}, "-root", root, "-system_log=false", "-noconfirm", "installed")
		if code != 1 {
			t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
		}
		if !strings.Contains(out, "injected prepareRoot failure") {
			t.Errorf("output = %q, want prepareRoot error", out)
		}
		if strings.Contains(out, "panic") || strings.Contains(out, "Error unmarshalling conf file") {
			t.Errorf("output unexpectedly read googet.conf before prepareRoot:\n%s", out)
		}
	})

	t.Run("prepareRoot repairs root before loadConf reads googet.conf", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "googet.conf"), 0755); err != nil {
			t.Fatal(err)
		}
		code, out := runGoogetEnv(t, true, []string{
			childPrepareModeEnv + "=repair_conf",
		}, "-root", root, "-system_log=false", "-noconfirm", "installed", "-format=json")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		if strings.Contains(out, "panic") || strings.Contains(out, "Error unmarshalling conf file") {
			t.Errorf("output unexpectedly reported conf error:\n%s", out)
		}
	})
}

func TestRunCallOrder(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		admin    bool
		root     func(t *testing.T) string
		args     []string
		elevated bool
		wantCode int
		want     []string
	}{
		{
			desc:     "privileged path loads googet.conf after the checks",
			admin:    true,
			root:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "googet") },
			args:     []string{"listrepos"},
			elevated: true,
			wantCode: 0,
			want: []string{
				"isAdmin",
				"prepareRoot:repos,cache",
				"checkEntries:pre-lock",
				"loadConf",
				"obtainLock",
				"checkEntries:post-lock",
				"rotateLog",
				"createDB",
				"mkdirAll:cache",
				"mkdirAll:repos",
				"execute",
			},
		},
		{
			desc:     "listrepos without the admin check loads googet.conf first",
			root:     func(t *testing.T) string { return t.TempDir() },
			args:     []string{"listrepos"},
			wantCode: 0,
			want:     []string{"loadConf", "execute"},
		},
		{
			desc: "installed without the admin check loads googet.conf first",
			root: func(t *testing.T) string {
				root := t.TempDir()
				db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
				if err != nil {
					t.Fatalf("creating test db: %v", err)
				}
				db.Close()
				return root
			},
			args:     []string{"installed", "-format=json"},
			wantCode: 0,
			want:     []string{"loadConf", "execute"},
		},
		{
			desc:     "failed admin check doesn't load googet.conf",
			root:     func(t *testing.T) string { return t.TempDir() },
			args:     []string{"install", "foo"},
			wantCode: 1,
			want:     []string{"isAdmin"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.elevated {
				skipUnlessElevatedOnWindows(t)
			}
			trace := filepath.Join(t.TempDir(), "trace.txt")
			args := append([]string{"-root", tc.root(t), "-system_log=false", "-noconfirm"}, tc.args...)
			code, out := runGoogetEnv(t, tc.admin, []string{childTraceEnv + "=" + trace}, args...)
			if code != tc.wantCode {
				t.Errorf("googet %q exit code = %d, want %d; output:\n%s", tc.args, code, tc.wantCode, out)
			}
			if diff := cmp.Diff(tc.want, readTrace(t, trace)); diff != "" {
				t.Errorf("googet %q calls mismatch (-want +got):\n%s\noutput:\n%s", tc.args, diff, out)
			}
		})
	}
}

func TestRunQuarantinesUntrustedConfAndRepo(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("googet checks the files in its root only on Windows")
	}
	if system.IsAdmin() != nil {
		if os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatal("this test needs an elevated process")
		}
		t.Skip("this test needs an elevated process")
	}
	root := filepath.Join(t.TempDir(), "googet")
	args := []string{"-root", root, "-system_log=false", "-noconfirm", "clean"}
	if code, out := runGooget(t, true, args...); code != 0 {
		t.Fatalf("googet clean on a new root exit code = %d, want 0; output:\n%s", code, out)
	}
	conf := filepath.Join(root, "googet.conf")
	repos := filepath.Join(root, "repos")
	repoFile := filepath.Join(repos, "x.repo")
	for p, data := range map[string]string{conf: "lockfilemaxage: 1h\n", repoFile: "- name: x\n  url: https://example.com/x\n"} {
		if err := os.WriteFile(p, []byte(data), 0666); err != nil {
			t.Fatal(err)
		}
		// icacls enables the restore privilege to set another owner.
		if out, err := exec.Command("icacls", p, "/setowner", "*S-1-5-32-545").CombinedOutput(); err != nil {
			t.Fatalf("icacls %q /setowner: %v\n%s", p, err, out)
		}
	}
	if rfs, err := repo.ConfigFiles(repos); err != nil || len(rfs) != 1 {
		t.Fatalf("precondition: repo.ConfigFiles(%q) = %v, %v; want x.repo", repos, rfs, err)
	}

	trace := filepath.Join(t.TempDir(), "trace.txt")
	code, out := runGoogetEnv(t, true, []string{childTraceEnv + "=" + trace}, args...)
	if code != 1 {
		t.Errorf("googet clean with untrusted files exit code = %d, want 1; output:\n%s", code, out)
	}
	calls := readTrace(t, trace)
	for _, forbidden := range []string{"loadConf", "createDB", "mkdirAll:cache", "mkdirAll:repos", "execute"} {
		for _, got := range calls {
			if got == forbidden {
				t.Errorf("googet clean with untrusted conf called %q, want it skipped; calls: %q", forbidden, calls)
			}
		}
	}
	for _, p := range []string{conf, repoFile} {
		if !strings.Contains(out, p) {
			t.Errorf("googet clean output doesn't mention %q:\n%s", p, out)
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%q still exists (Lstat error: %v)", p, err)
		}
		if m, _ := filepath.Glob(p + ".untrusted-*"); len(m) != 1 {
			t.Errorf("renamed copies of %q = %q, want one", p, m)
		}
	}
	if rfs, err := repo.ConfigFiles(repos); err != nil || len(rfs) != 0 {
		t.Errorf("repo.ConfigFiles(%q) = %v, %v; want no repo files", repos, rfs, err)
	}
	if log, err := os.ReadFile(filepath.Join(root, "googet.log")); err != nil || !strings.Contains(string(log), conf) {
		t.Errorf("googet.log = %q, %v; want it to mention %q", log, err, conf)
	}

	if code, out := runGooget(t, true, args...); code != 0 {
		t.Errorf("googet clean after the files were renamed exit code = %d, want 0; output:\n%s", code, out)
	}
}

func recordedLockAge(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), lockAgePrefix); ok {
			return v
		}
	}
	return ""
}

func TestRunConfQuarantineSkipsLoadConfAndLogsFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	if err := os.MkdirAll(root, 0774); err != nil {
		t.Fatal(err)
	}
	// Leave a googet.conf with a custom lockfilemaxage in place; because
	// checkEntries reports a pre-lock failure, LoadConf must not be called.
	if err := os.WriteFile(filepath.Join(root, "googet.conf"), []byte("lockfilemaxage: 1m\n"), 0666); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "trace.txt")
	code, out := runGoogetEnv(t, true, []string{
		childPrepareModeEnv + "=created",
		childCheckModeEnv + "=lock-fail",
		childTraceEnv + "=" + trace,
		childRecordLockAgeEnv + "=1",
	}, "-root", root, "-system_log=false", "-noconfirm", "clean")
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
	}
	wantCalls := []string{
		"isAdmin",
		"prepareRoot:repos,cache",
		"checkEntries:pre-lock",
		"obtainLock",
		"checkEntries:post-lock",
		"rotateLog",
	}
	if diff := cmp.Diff(wantCalls, readTrace(t, trace)); diff != "" {
		t.Errorf("call order mismatch (-want +got):\n%s\noutput:\n%s", diff, out)
	}
	if got := recordedLockAge(out); got != (24 * time.Hour).String() {
		t.Errorf("obtainLock maxAge = %q, want default %q when conf failed check", got, (24 * time.Hour).String())
	}
	logBytes, err := os.ReadFile(filepath.Join(root, "googet.log"))
	if err != nil {
		t.Fatalf("reading googet.log: %v", err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "Removed or renamed entries in the googet root") {
		t.Errorf("googet.log missing change warning; got:\n%s", logText)
	}
	if !strings.Contains(logText, "googet.conf.untrusted-1") {
		t.Errorf("googet.log missing quarantine failure; got:\n%s", logText)
	}
	if _, err := os.Lstat(filepath.Join(root, "googet.db")); !os.IsNotExist(err) {
		t.Errorf("googet.db was created after pre-lock entry check failure (Lstat error: %v)", err)
	}
}

func TestRunHonorsLockFileMaxAgeFromTrustedConf(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	if err := os.MkdirAll(root, 0774); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "googet.conf"), []byte("lockfilemaxage: 42m\n"), 0666); err != nil {
		t.Fatal(err)
	}
	code, out := runGoogetEnv(t, true, []string{
		childPrepareModeEnv + "=created",
		childRecordLockAgeEnv + "=1",
	}, "-root", root, "-system_log=false", "-noconfirm", "clean")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if got := recordedLockAge(out); got != (42 * time.Minute).String() {
		t.Errorf("obtainLock maxAge = %q, want %q from googet.conf", got, (42 * time.Minute).String())
	}
}

func TestRunPostLockFailureAndCheckErrorsStopEarly(t *testing.T) {
	for _, tc := range []struct {
		desc        string
		prepareMode string
		mode        string
		wantCalls   []string
		wantLog     string
		wantOut     string
	}{
		{
			desc:        "post-lock failure logs error and exits before createDB",
			prepareMode: "created",
			mode:        "post-fail",
			wantCalls: []string{
				"isAdmin",
				"prepareRoot:repos,cache",
				"checkEntries:pre-lock",
				"loadConf",
				"obtainLock",
				"checkEntries:post-lock",
				"rotateLog",
			},
			wantLog: "has an untrusted owner",
		},
		{
			desc:        "pre-lock check error stops before loadConf and obtainLock and still logs root repair",
			prepareMode: "repaired",
			mode:        "lock-err",
			wantCalls: []string{
				"isAdmin",
				"prepareRoot:repos,cache",
				"checkEntries:pre-lock",
			},
			wantOut: "Reset the permissions of the googet root",
		},
		{
			desc:        "post-lock check error stops before rotateLog and createDB and still logs root repair",
			prepareMode: "repaired",
			mode:        "post-err",
			wantCalls: []string{
				"isAdmin",
				"prepareRoot:repos,cache",
				"checkEntries:pre-lock",
				"loadConf",
				"obtainLock",
				"checkEntries:post-lock",
			},
			wantOut: "Reset the permissions of the googet root",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			trace := filepath.Join(t.TempDir(), "trace.txt")
			code, out := runGoogetEnv(t, true, []string{
				childPrepareModeEnv + "=" + tc.prepareMode,
				childCheckModeEnv + "=" + tc.mode,
				childTraceEnv + "=" + trace,
			}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
			if code != 1 {
				t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
			}
			if diff := cmp.Diff(tc.wantCalls, readTrace(t, trace)); diff != "" {
				t.Errorf("call order mismatch (-want +got):\n%s\noutput:\n%s", diff, out)
			}
			if tc.wantLog != "" {
				logBytes, err := os.ReadFile(filepath.Join(root, "googet.log"))
				if err != nil {
					t.Fatalf("reading googet.log: %v", err)
				}
				if !strings.Contains(string(logBytes), tc.wantLog) {
					t.Errorf("googet.log = %q, want it to contain %q", logBytes, tc.wantLog)
				}
			}
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out, tc.wantOut)
			}
			if _, err := os.Lstat(filepath.Join(root, "googet.db")); !os.IsNotExist(err) {
				t.Errorf("googet.db was created despite %s (Lstat error: %v)", tc.mode, err)
			}
		})
	}
}

func TestRunUntrustedStateFailsBeforeCreateDB(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("googet checks the files in its root only on Windows")
	}
	if system.IsAdmin() != nil {
		if os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatal("this test needs an elevated process")
		}
		t.Skip("this test needs an elevated process")
	}
	for _, name := range []string{"googet.state", "googet.state.bak"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			if _, err := system.PrepareRoot(root, []string{"repos", "cache"}); err != nil {
				t.Fatalf("PrepareRoot(%q): %v", root, err)
			}
			stateFile := filepath.Join(root, name)
			if err := os.WriteFile(stateFile, []byte("[]"), 0666); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("icacls", stateFile, "/setowner", "*S-1-5-32-545").CombinedOutput(); err != nil {
				t.Fatalf("icacls %q /setowner: %v\n%s", stateFile, err, out)
			}
			trace := filepath.Join(t.TempDir(), "trace.txt")
			code, out := runGoogetEnv(t, true, []string{childTraceEnv + "=" + trace}, "-root", root, "-system_log=false", "-noconfirm", "clean")
			if code != 1 {
				t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
			}
			calls := readTrace(t, trace)
			for _, forbidden := range []string{"createDB", "mkdirAll:cache", "mkdirAll:repos", "execute"} {
				for _, got := range calls {
					if got == forbidden {
						t.Errorf("run with untrusted %s called %q, want it skipped; calls: %q", name, forbidden, calls)
					}
				}
			}
			if _, err := os.Lstat(filepath.Join(root, "googet.db")); !os.IsNotExist(err) {
				t.Errorf("googet.db was created from untrusted %s (Lstat error: %v)", name, err)
			}
			if got, err := os.ReadFile(stateFile); err != nil || string(got) != "[]" {
				t.Errorf("%s = %q, %v; want it left untouched", name, got, err)
			}
		})
	}
}
