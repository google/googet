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

package goolib

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/googet/v2/supervisor"
)

func TestScriptInterpreter(t *testing.T) {
	table := []struct {
		script string
		eitp   string
	}{
		{"/file/path/script.ps1", "powershell"},
		{"/file/path/script.cmd", "cmd"},
		{"/file/path/script.bat", "cmd"},
	}
	for _, tt := range table {
		itp, err := scriptInterpreter(tt.script)
		if err != nil {
			t.Errorf("error parsing interpreter: %v", err)
		}
		if itp != tt.eitp {
			t.Errorf("did not get expected interpreter: got %v, want %v", itp, tt.eitp)
		}
	}
}

func TestBadScriptInterpreter(t *testing.T) {
	if _, err := scriptInterpreter("/file/path/script.ext"); err == nil {
		t.Errorf("got no error from scriptInterpreter when processing bad extension, want error")
	}
	if _, err := scriptInterpreter("/file/path/script"); err == nil {
		t.Errorf("got no error from scriptInterpreter when processing no extension, want error")
	}
}

func randString(runes []rune, min, max int) string {
	s := make([]rune, rand.Intn(1+max-min)+min)
	for i := range s {
		s[i] = runes[rand.Intn(len(runes))]
	}
	return string(s)
}

func TestSplitGCSUrl(t *testing.T) {
	const alphanum = "abcdefghijklmnopqrstuvwxyz0123456789"
	objChars := alphanum + "ABCDEFGHIJKLMNOPQRSTUVWXYZ-_.~@%^=+"
	bucket := randString([]rune(alphanum), 1, 1) + randString([]rune(alphanum+"-_."), 0, 61) + randString([]rune(alphanum), 1, 1)
	object := randString([]rune(objChars+"/"), 0, 49) + randString([]rune(objChars), 1, 1)

	var domains = []string{
		`storage.cloud.google.com`,
		`storage.googleapis.com`,
		`commondatastorage.googleapis.com`,
	}
	var urls, urlsNoObjs []string
	for i := range domains {
		for _, s := range []string{"", "s"} {
			// Without Objects
			urlsNoObjs = append(urlsNoObjs, fmt.Sprintf("http%s://%s/%s", s, domains[i], bucket))
			urlsNoObjs = append(urlsNoObjs, fmt.Sprintf("http%s://%s/%s", s, strings.ToUpper(domains[i]), bucket))
			// With objects
			urls = append(urls, fmt.Sprintf("http%s://%s/%s/%s", s, domains[i], bucket, object))
			urls = append(urls, fmt.Sprintf("http%s://%s/%s/%s", s, strings.ToUpper(domains[i]), bucket, object))
		}
	}
	urls = append(urls, fmt.Sprintf(`http://%s.storage.googleapis.com/%s`, bucket, object))
	urls = append(urls, fmt.Sprintf(`http://%s.Storage.googleapis.COM/%s`, bucket, object))
	urls = append(urls, fmt.Sprintf(`https://%s.storage.googleapis.com/%s`, bucket, object))
	urls = append(urls, fmt.Sprintf(`https://%s.Storage.googleapis.COM/%s`, bucket, object))
	urlsNoObjs = append(urlsNoObjs, fmt.Sprintf(`gs://%s`, bucket))
	urls = append(urls, fmt.Sprintf(`gs://%s/%s`, bucket, object))
	for i := range urls {
		urls = append(urls, urls[i]+"/")
	}
	for _, url := range urls {
		ok := true
		isGCSUrl, bkt, obj := SplitGCSUrl(url)
		if !isGCSUrl {
			t.Errorf("Failed to parse '%s', expecting bucket='%s', object='%s'", url, bucket, object)
			ok = false
		} else {
			if bkt != bucket {
				t.Errorf("Parsed bucket '%s' from '%s', expecting '%s'", bkt, url, bucket)
				ok = false
			}
			if obj != object {
				t.Errorf("Parsed object '%s' from '%s', expecting '%s'", obj, url, object)
				ok = false
			}
		}
		if ok {
			t.Logf("Successfully parsed object='%s', bucket='%s' from '%s'", obj, bkt, url)
		}
	}
	for _, url := range urlsNoObjs {
		ok := true
		isGCSUrl, bkt, obj := SplitGCSUrl(url)
		if !isGCSUrl {
			t.Errorf("Failed to parse '%s', expecting bucket='%s', no object", url, bucket)
			ok = false
		} else {
			if bkt != bucket {
				t.Errorf("Parsed bucket '%s' from '%s', expecting '%s'", bkt, url, bucket)
				ok = false
			}
			if obj != "" {
				t.Errorf("Parsed object '%s' from '%s', expecting an empty string", obj, url)
				ok = false
			}
		}
		if ok {
			t.Logf("Successfully parsed object='%s', bucket='%s' from '%s'", obj, bkt, url)
		}
	}
}

// TestEnrichOptionsIgnoresOSArgs verifies that unattended mode is never inferred from os.Args.
func TestEnrichOptionsIgnoresOSArgs(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	for _, init := range []bool{false, true} {
		os.Args = []string{"googet", "-noconfirm", "/noconfirm", "install", "noconfirm"}
		opts := enrichOptions(exec.Command("cmd.exe", "/c", "echo"), supervisor.Options{Unattended: init}, nil)
		if opts.Unattended != init {
			t.Errorf("enrichOptions(Unattended=%v) = %v, want %v", init, opts.Unattended, init)
		}
	}
}

// TestIsLogFlag verifies recognition of msiexec and common installer logging switches.
func TestIsLogFlag(t *testing.T) {
	for _, s := range []string{"/log", "-LOG", "--log", "/l", "-l", "/l*v", "/L*V", "/lv*", "/l*vx", "/l+!", "/liwe"} {
		if !isLogFlag(s) {
			t.Errorf("isLogFlag(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "/", "l*v", "/lang", "/qn", "/i", "-lz", "/logs", "C:\\x.log"} {
		if isLogFlag(s) {
			t.Errorf("isLogFlag(%q) = true, want false", s)
		}
	}
}

// TestLogFlagParsing verifies colon-delimited and space-separated log flag parsing.
func TestLogFlagParsing(t *testing.T) {
	tests := []struct {
		name     string
		cmdArgs  []string
		initLogs []string
		wantLogs []string
	}{
		{
			name:     "colon-delimited /log:<path>",
			cmdArgs:  []string{"msiexec", "/i", "pkg.msi", `/log:C:\install.log`},
			wantLogs: []string{`C:\install.log`},
		},
		{
			name:     "colon-delimited /l*v:<path>",
			cmdArgs:  []string{"msiexec", "/i", "pkg.msi", `/l*v:C:\msi.log`},
			wantLogs: []string{`C:\msi.log`},
		},
		{
			name:     "colon-delimited -log:<path>",
			cmdArgs:  []string{"setup.exe", `-log:C:\boot.log`},
			wantLogs: []string{`C:\boot.log`},
		},
		{
			name:     "colon-delimited -l:<path>",
			cmdArgs:  []string{"setup.exe", `-l:C:\app.log`},
			wantLogs: []string{`C:\app.log`},
		},
		{
			name:     "colon-delimited /l:<path>",
			cmdArgs:  []string{"msiexec", "/i", "pkg.msi", `/l:C:\quick.log`},
			wantLogs: []string{`C:\quick.log`},
		},
		{
			name:     "colon-delimited --log:<path>",
			cmdArgs:  []string{"wix.exe", `--log:C:\wix.log`},
			wantLogs: []string{`C:\wix.log`},
		},
		{
			name:     "colon-delimited /l*vx:<path>",
			cmdArgs:  []string{"msiexec", `/l*vx:C:\verbose.log`},
			wantLogs: []string{`C:\verbose.log`},
		},
		{
			name:     "colon-delimited -l*vx:<path>",
			cmdArgs:  []string{"msiexec", `-l*vx:C:\verbose2.log`},
			wantLogs: []string{`C:\verbose2.log`},
		},
		{
			name:     "colon-delimited -l*v:<path>",
			cmdArgs:  []string{"msiexec", `-l*v:C:\verbose3.log`},
			wantLogs: []string{`C:\verbose3.log`},
		},
		{
			name:     "colon-delimited double-quoted path",
			cmdArgs:  []string{"msiexec", `/log:"C:\Program Files\App\install.log"`},
			wantLogs: []string{`C:\Program Files\App\install.log`},
		},
		{
			name:     "colon-delimited single-quoted path",
			cmdArgs:  []string{"msiexec", `/l*v:'C:\Logs\test.log'`},
			wantLogs: []string{`C:\Logs\test.log`},
		},
		{
			name:     "colon-delimited uppercase /LOG with original path casing preserved",
			cmdArgs:  []string{"msiexec", `/LOG:C:\MyFolder\Install.LOG`},
			wantLogs: []string{`C:\MyFolder\Install.LOG`},
		},
		{
			name:     "colon-delimited empty path ignored",
			cmdArgs:  []string{"msiexec", "/log:"},
			wantLogs: nil,
		},
		{
			name:     "colon-delimited quoted empty path ignored",
			cmdArgs:  []string{"msiexec", `/log:""`},
			wantLogs: nil,
		},
		{
			name:     "colon-delimited with spaces around quotes",
			cmdArgs:  []string{"msiexec", `/log:  "C:\Logs\install.log"  `},
			wantLogs: []string{`C:\Logs\install.log`},
		},
		{
			name:     "colon-delimited with multiple colons in path",
			cmdArgs:  []string{"setup.exe", `/log:C:\foo:bar\baz.log`},
			wantLogs: []string{`C:\foo:bar\baz.log`},
		},
		{
			name:     "space-separated /log <path>",
			cmdArgs:  []string{"msiexec", "/i", "pkg.msi", "/log", `C:\install.log`},
			wantLogs: []string{`C:\install.log`},
		},
		{
			name:     "space-separated /l*v <path>",
			cmdArgs:  []string{"msiexec", "/i", "pkg.msi", "/l*v", `C:\msi.log`},
			wantLogs: []string{`C:\msi.log`},
		},
		{
			name:     "space-separated -log <path>",
			cmdArgs:  []string{"setup.exe", "-log", `C:\boot.log`},
			wantLogs: []string{`C:\boot.log`},
		},
		{
			name:     "space-separated -l <path>",
			cmdArgs:  []string{"setup.exe", "-l", `C:\app.log`},
			wantLogs: []string{`C:\app.log`},
		},
		{
			name:     "space-separated /l <path>",
			cmdArgs:  []string{"msiexec", "/l", `C:\quick.log`},
			wantLogs: []string{`C:\quick.log`},
		},
		{
			name:     "space-separated --log <path>",
			cmdArgs:  []string{"wix.exe", "--log", `C:\wix.log`},
			wantLogs: []string{`C:\wix.log`},
		},
		{
			name:     "space-separated /l*vx <path>",
			cmdArgs:  []string{"msiexec", "/l*vx", `C:\verbose.log`},
			wantLogs: []string{`C:\verbose.log`},
		},
		{
			name:     "space-separated -l*vx <path>",
			cmdArgs:  []string{"msiexec", "-l*vx", `C:\verbose2.log`},
			wantLogs: []string{`C:\verbose2.log`},
		},
		{
			name:     "space-separated -l*v <path>",
			cmdArgs:  []string{"msiexec", "-l*v", `C:\verbose3.log`},
			wantLogs: []string{`C:\verbose3.log`},
		},
		{
			name:     "space-separated double-quoted path",
			cmdArgs:  []string{"msiexec", "/log", `"C:\Program Files\App\install.log"`},
			wantLogs: []string{`C:\Program Files\App\install.log`},
		},
		{
			name:     "space-separated single-quoted path",
			cmdArgs:  []string{"msiexec", "/l*v", `'C:\Logs\test.log'`},
			wantLogs: []string{`C:\Logs\test.log`},
		},
		{
			name:     "space-separated uppercase /LOG",
			cmdArgs:  []string{"setup.exe", "/LOG", `C:\install.log`},
			wantLogs: []string{`C:\install.log`},
		},
		{
			name:     "space-separated uppercase /L*V",
			cmdArgs:  []string{"msiexec", "/L*V", `C:\msi.log`},
			wantLogs: []string{`C:\msi.log`},
		},
		{
			name:     "space-separated trailing flag without path",
			cmdArgs:  []string{"msiexec", "/log"},
			wantLogs: nil,
		},
		{
			name:     "space-separated with empty string value",
			cmdArgs:  []string{"msiexec", "/log", ""},
			wantLogs: nil,
		},
		{
			name:     "space-separated with quoted empty value",
			cmdArgs:  []string{"msiexec", "/log", `""`},
			wantLogs: nil,
		},
		{
			name:     "multiple mixed log flags",
			cmdArgs:  []string{"installer.exe", `/l*v:C:\first.log`, "-log", `C:\second.log`},
			wantLogs: []string{`C:\first.log`, `C:\second.log`},
		},
		{
			name:     "deduplicate identical log files",
			cmdArgs:  []string{"installer.exe", `/log:C:\dup.log`, "/log", `C:\dup.log`},
			wantLogs: []string{`C:\dup.log`},
		},
		{
			name:     "preserve pre-existing LogFiles",
			cmdArgs:  []string{"installer.exe", `/log:C:\new.log`},
			initLogs: []string{`C:\existing.log`},
			wantLogs: []string{`C:\existing.log`, `C:\new.log`},
		},
		{
			name:     "do not duplicate pre-existing LogFiles",
			cmdArgs:  []string{"installer.exe", `/log:C:\existing.log`},
			initLogs: []string{`C:\existing.log`},
			wantLogs: []string{`C:\existing.log`},
		},
		{
			name:     "consecutive log flags does not consume next flag as path",
			cmdArgs:  []string{"installer.exe", "/log", "/l*v", `C:\real.log`},
			wantLogs: []string{`C:\real.log`},
		},
		{
			name:     "non-flag colon argument not misidentified",
			cmdArgs:  []string{"copy.exe", `C:\source.txt`, `D:\dest.txt`},
			wantLogs: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &exec.Cmd{Args: tc.cmdArgs}
			opts := enrichOptions(cmd, supervisor.Options{LogFiles: tc.initLogs}, nil)
			if !reflect.DeepEqual(opts.LogFiles, tc.wantLogs) {
				t.Errorf("enrichOptions() LogFiles = %v, want %v", opts.LogFiles, tc.wantLogs)
			}
		})
	}
}

// syncBuffer is a bytes.Buffer that is safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureStdio runs fn with os.Stdout and os.Stderr redirected to pipes and
// returns what was written to each.
func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	capture := func(f **os.File) (restore func() string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		orig := *f
		*f = w
		done := make(chan string)
		go func() {
			b, _ := io.ReadAll(r)
			done <- string(b)
		}()
		return func() string {
			w.Close()
			*f = orig
			return <-done
		}
	}
	restoreOut := capture(&os.Stdout)
	restoreErr := capture(&os.Stderr)
	fn()
	return restoreOut(), restoreErr()
}

func TestRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	for _, tt := range []struct {
		name    string
		script  string
		ec      []int
		wantErr bool
	}{
		{"success", "echo out; echo err >&2", nil, false},
		{"accepted exit code", "echo out; echo err >&2; exit 3", []int{3}, false},
		{"rejected exit code", "echo out; echo err >&2; exit 3", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Run writes stdout and stderr to w from separate goroutines, so
			// the shared writer must be safe for concurrent use.
			var w syncBuffer
			var err error
			stdout, stderr := captureStdio(t, func() {
				err = Run(exec.Command("/bin/sh", "-c", tt.script), tt.ec, &w)
			})
			if (err != nil) != tt.wantErr {
				t.Errorf("Run() error = %v, wantErr %v", err, tt.wantErr)
			}
			// Without an active spinner the child's output must reach the
			// process's own stdout and stderr unchanged, so unattended callers
			// see exactly what they saw before progress reporting existed.
			if stdout != "out\n" {
				t.Errorf("stdout = %q, want %q", stdout, "out\n")
			}
			if stderr != "err\n" {
				t.Errorf("stderr = %q, want %q", stderr, "err\n")
			}
			if got := w.String(); !strings.Contains(got, "out\n") || !strings.Contains(got, "err\n") {
				t.Errorf("writer got %q, want both stdout and stderr lines", got)
			}
		})
	}
}

// TestEnrichOptionsNilAndWriter verifies safety when inspecting nil commands and writers.
func TestEnrichOptionsNilAndWriter(t *testing.T) {
	// Nil command safety check.
	optsNilCmd := enrichOptions(nil, supervisor.Options{}, nil)
	if optsNilCmd.LogFiles != nil {
		t.Errorf("enrichOptions(nil, ...) LogFiles = %v, want nil", optsNilCmd.LogFiles)
	}

	// Command with nil Args slice.
	optsNilArgs := enrichOptions(&exec.Cmd{Args: nil}, supervisor.Options{}, nil)
	if optsNilArgs.LogFiles != nil {
		t.Errorf("enrichOptions(&exec.Cmd{Args: nil}, ...) LogFiles = %v, want nil", optsNilArgs.LogFiles)
	}

	// Test writer inspection with a temporary file.
	tmpFile, err := os.CreateTemp("", "googet_enrich_test_*.log")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	optsFile := enrichOptions(nil, supervisor.Options{}, tmpFile)
	if len(optsFile.LogFiles) != 1 || optsFile.LogFiles[0] != tmpFile.Name() {
		t.Errorf("enrichOptions(nil, ..., tmpFile) LogFiles = %v, want [%v]", optsFile.LogFiles, tmpFile.Name())
	}

	// Pre-existing log file should not be duplicated when writer is inspected.
	optsDupFile := enrichOptions(nil, supervisor.Options{LogFiles: []string{tmpFile.Name()}}, tmpFile)
	if len(optsDupFile.LogFiles) != 1 || optsDupFile.LogFiles[0] != tmpFile.Name() {
		t.Errorf("enrichOptions(nil, ..., tmpFile) with pre-existing LogFiles = %v, want [%v]", optsDupFile.LogFiles, tmpFile.Name())
	}
}

// TestAdversarialCornerCases documents empirical edge-case behavior and limitations of enrichOptions.
func TestAdversarialCornerCases(t *testing.T) {
	// 1. InnoSetup-style /LOG=<path> is currently unhandled by enrichOptions.
	// When installers use /LOG=path, the colon splitter does not split on '='.
	// Therefore, the log path is not auto-discovered.
	cmdInno := &exec.Cmd{Args: []string{"setup.exe", `/VERYSILENT`, `/LOG=C:\Windows\Logs\inno.log`}}
	optsInno := enrichOptions(cmdInno, supervisor.Options{}, nil)
	if len(optsInno.LogFiles) != 0 {
		t.Logf("Notice: /LOG= was unexpectedly parsed as %v", optsInno.LogFiles)
	} else {
		t.Logf("Empirically confirmed: InnoSetup /LOG= syntax is unhandled by enrichOptions (LogFiles is empty)")
	}

	// 2. Fully-quoted argument "/log:path" has leading quote on the switch.
	// strings.SplitN produces parts[0] == `"/log`, which fails isLogFlag.
	cmdQuotedSwitch := &exec.Cmd{Args: []string{"setup.exe", `"/log:C:\install.log"`}}
	optsQuotedSwitch := enrichOptions(cmdQuotedSwitch, supervisor.Options{}, nil)
	if len(optsQuotedSwitch.LogFiles) != 0 {
		t.Logf("Notice: Fully-quoted switch was parsed as %v", optsQuotedSwitch.LogFiles)
	} else {
		t.Logf("Empirically confirmed: Fully-quoted switch \"/log:...\" is not extracted (LogFiles is empty)")
	}

	// 3. Space-separated log flag followed by another command switch (/quiet).
	// Because /quiet is not a known log flag, isLogFlag("/quiet") returns false.
	// This causes enrichOptions to treat "/quiet" as the log file path.
	cmdNextSwitch := &exec.Cmd{Args: []string{"msiexec.exe", "/i", "pkg.msi", "/log", "/quiet"}}
	optsNextSwitch := enrichOptions(cmdNextSwitch, supervisor.Options{}, nil)
	if len(optsNextSwitch.LogFiles) == 1 && optsNextSwitch.LogFiles[0] == "/quiet" {
		t.Logf("Empirically confirmed: /log followed by non-log switch treats switch (/quiet) as log path")
	}

	// 4. Bare argument "noconfirm" without dash or slash prefix in os.Args.
	// strings.TrimLeft(arg, "-/") strips leading prefixes, but if none exist, clean == "noconfirm".
	// This inadvertently sets opts.Unattended = true.
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"googet", "install", "noconfirm"}
	optsBare := enrichOptions(&exec.Cmd{Args: []string{"cmd.exe"}}, supervisor.Options{}, nil)
	if optsBare.Unattended {
		t.Logf("Empirically confirmed: Package named 'noconfirm' in os.Args triggers opts.Unattended = true")
	}

	// 5. Forward slash paths in Windows commands: /log:C:/temp/install.log.
	cmdFwd := &exec.Cmd{Args: []string{"setup.exe", `/log:C:/temp/install.log`}}
	optsFwd := enrichOptions(cmdFwd, supervisor.Options{}, nil)
	if len(optsFwd.LogFiles) != 1 || optsFwd.LogFiles[0] != "C:/temp/install.log" {
		t.Errorf("enrichOptions() with forward-slash path = %v, want [C:/temp/install.log]", optsFwd.LogFiles)
	}
}
