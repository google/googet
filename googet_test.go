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
	"encoding/json"
	"errors"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/oswrap"
)

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
