/*
Copyright 2018 Google Inc. All Rights Reserved.
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

package verify

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
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

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

func TestMain(m *testing.M) {
	// Initialize the logger.
	logger.Init("test", false, false, ioutil.Discard)
	os.Exit(m.Run())
}

func TestFiles(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	testFile := filepath.Join(tempDir, "test")
	f, err := os.Create(testFile)
	if err != nil {
		t.Fatalf("error creating temp file: %v", err)
	}
	chksm := goolib.Checksum(f)
	if err := f.Close(); err != nil {
		t.Fatalf("error saving temp file: %v", err)
	}

	table := []struct {
		testCase string
		ps       client.PackageState
		verify   bool
	}{
		{"no files at all", client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, true},
		{"no files found", client.PackageState{InstalledFiles: map[string]string{"foo": "bar"}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, false},
		{"file checksum does not match", client.PackageState{InstalledFiles: map[string]string{testFile: "bar"}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, false},
		{"file checksum matches", client.PackageState{InstalledFiles: map[string]string{testFile: chksm}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, true},
		{"should skip folder", client.PackageState{InstalledFiles: map[string]string{tempDir: "", testFile: chksm}, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1.0.0@1"}}, true},
	}
	for _, tt := range table {
		verify, err := Files(tt.ps)
		if err != nil {
			t.Errorf("%q: error running verify: %v", tt.testCase, err)
		}
		if verify != tt.verify {
			t.Errorf("%q: unexpected verification result, want: %t, got: %t", tt.testCase, tt.verify, verify)
		}
	}
}

func writeVerifyGoo(t *testing.T, gooPath, script string) goolib.PkgSpec {
	t.Helper()
	ps := goolib.PkgSpec{
		Name:    "foo",
		Arch:    "noarch",
		Version: "1.0.0@1",
		Verify:  goolib.ExecFile{Path: "verify.sh"},
	}
	specJSON, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(gooPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for _, entry := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"verify.sh", 0755, []byte(script)},
		{"extra", 0644, []byte("x")},
		{"foo.pkgspec", 0644, specJSON},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), gw.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestCommandInterrupted(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux shell script and Unix signal forwarding")
	}
	const envKey = "GOOGET_VERIFY_INTERRUPT_MODE"
	if mode := os.Getenv(envKey); mode != "" {
		dir := t.TempDir()
		gooPath := filepath.Join(dir, "foo.noarch.1.0.0@1.goo")
		script := "#!/bin/sh\nkill -TERM $PPID\nsleep 10\n"
		switch mode {
		case "retry":
			// Fail the initial single-file run so Command extracts the full
			// package and runs the second system.Verify call.
			script = "#!/bin/sh\n[ -f \"$(dirname \"$0\")/extra\" ] || exit 1\nkill -TERM $PPID\nsleep 10\n"
		case "after-exit":
			// Fail the initial single-file run and signal once Run has reaped
			// the shell while still draining stdout, so Command aborts before
			// extracting the full package.
			script = "#!/bin/sh\n(while kill -0 $$ 2>/dev/null; do sleep 0.01; done; kill -TERM $PPID; sleep 0.5) &\nexit 1\n"
		}
		ps := writeVerifyGoo(t, gooPath, script)

		state := client.PackageState{LocalPath: gooPath, PackageSpec: &ps}
		ok, err := Command(t.Context(), state, nil)
		if ok || !errors.Is(err, goolib.ErrInterrupted) {
			t.Fatalf("Command() = %v, %v, want false, %v", ok, err, goolib.ErrInterrupted)
		}
		if mode != "retry" {
			extractDir := strings.TrimSuffix(gooPath, ".goo")
			if _, serr := os.Stat(filepath.Join(extractDir, "extra")); !os.IsNotExist(serr) {
				t.Fatalf("Command() extracted full package after interrupt (stat err = %v)", serr)
			}
		}
		// Once interrupted, a later Command call must fail fast before pulling
		// a missing local package.
		missing := client.PackageState{LocalPath: filepath.Join(dir, "missing.goo"), PackageSpec: &ps}
		if ok, err := Command(t.Context(), missing, nil); ok || !errors.Is(err, goolib.ErrInterrupted) {
			t.Fatalf("second Command() = %v, %v, want false, %v", ok, err, goolib.ErrInterrupted)
		}
		return
	}

	for _, mode := range []string{"first", "retry", "after-exit"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCommandInterrupted$")
			cmd.Env = append(os.Environ(), envKey+"="+mode, "GORACE=atexit_sleep_ms=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("subprocess (%s) failed: %v\n%s", mode, err, out)
			}
		})
	}
}

func TestCommandTimeout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux shell script")
	}
	orig := goolib.Timeout
	t.Cleanup(func() { goolib.Timeout = orig })
	goolib.Timeout = 100 * time.Millisecond

	dir := t.TempDir()
	runsFile := filepath.Join(dir, "runs")
	gooPath := filepath.Join(dir, "foo.noarch.1.0.0@1.goo")
	ps := writeVerifyGoo(t, gooPath, fmt.Sprintf("#!/bin/sh\necho x >> %q\nsleep 10\n", runsFile))

	ok, err := Command(t.Context(), client.PackageState{LocalPath: gooPath, PackageSpec: &ps}, nil)
	if ok || err != nil {
		t.Fatalf("Command() = %v, %v, want false, nil", ok, err)
	}
	runs, rerr := os.ReadFile(runsFile)
	if rerr != nil || string(runs) != "x\n" {
		t.Errorf("verify script runs = %q (err %v), want exactly 1 run", runs, rerr)
	}
	extractDir := strings.TrimSuffix(gooPath, ".goo")
	if _, serr := os.Stat(filepath.Join(extractDir, "extra")); !os.IsNotExist(serr) {
		t.Errorf("Command() extracted full package after timeout (stat err = %v)", serr)
	}
}

func TestRetryFromFullPackage(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"exit failure", errors.New("command exited with error code 1"), true},
		{"interrupted", fmt.Errorf("wrap: %w", goolib.ErrInterrupted), false},
		{"timeout", fmt.Errorf("wrap: %w", goolib.ErrTimeout), false},
		{"inactive", fmt.Errorf("wrap: %w", goolib.ErrInactive), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryFromFullPackage(tt.err); got != tt.want {
				t.Errorf("retryFromFullPackage(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
