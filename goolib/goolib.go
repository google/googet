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

// Package goolib contains common functions useful when working with GooGet.
package goolib

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/google/googet/v2/supervisor"
)

var interpreter = map[string]string{
	".ps1": "powershell",
	".cmd": "cmd",
	".bat": "cmd",
	".exe": "cmd",
}

// scriptInterpreter reads a scripts extension and returns the interpreter to use.
func scriptInterpreter(s string) (string, error) {
	ext := filepath.Ext(s)
	itp, ok := interpreter[ext]
	if ok {
		return itp, nil
	}
	return "", fmt.Errorf("unknown extension %q", ext)
}

// Exec execs a script or binary on either Windows or Linux using the provided args.
// The process is successful if the exit code matches any of those provided or '0'.
// stdout and stderr are sent to the writer.
func Exec(s string, args []string, ec []int, w io.Writer) error {
	return ExecWithOptions(s, args, ec, supervisor.Options{}, w)
}

// ExecWithOptions executes a script or binary with custom supervisor options.
func ExecWithOptions(s string, args []string, ec []int, opts supervisor.Options, w io.Writer) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cs := filepath.Clean(s)
		ipr, err := scriptInterpreter(cs)
		if err != nil {
			return err
		}
		switch ipr {
		case "powershell":
			// We are using `-Command` here instead of `-File` as this catches syntax errors in the script.
			args = append([]string{"-ExecutionPolicy", "Bypass", "-NonInteractive", "-NoProfile", "-Command", cs}, args...)
			c = exec.Command(ipr, args...)
		case "cmd":
			c = exec.Command(cs, args...)
		default:
			return fmt.Errorf("unknown interpreter: %q", ipr)
		}
	case "linux":
		c = exec.Command(s, args...)
	default:
		return fmt.Errorf("OS %q is not Windows or Linux", runtime.GOOS)
	}
	return RunWithOptions(c, ec, opts, w)
}

// msiLogModifiers are the characters allowed after /l in msiexec logging switches (e.g. /l*v, /lv*x, /l+!).
const msiLogModifiers = "iwearucmopvx+!*"

// isLogFlag reports whether the given token matches a known installer logging flag.
func isLogFlag(s string) bool {
	ls := strings.ToLower(s)
	switch ls {
	case "/log", "-log", "--log":
		return true
	}
	if len(ls) < 2 || (ls[0] != '/' && ls[0] != '-') || ls[1] != 'l' {
		return false
	}
	for _, r := range ls[2:] {
		if !strings.ContainsRune(msiLogModifiers, r) {
			return false
		}
	}
	return true
}

// enrichOptions inspects command line arguments and the output writer to auto-discover
// log files whose growth indicates forward progress. Unattended mode is configured
// process-wide via supervisor.Configure and is not inferred here.
func enrichOptions(c *exec.Cmd, opts supervisor.Options, w io.Writer) supervisor.Options {
	seen := make(map[string]bool)
	for _, f := range opts.LogFiles {
		seen[f] = true
	}
	// addLog appends path to opts.LogFiles unless it is empty or already listed.
	addLog := func(path string) {
		if path != "" && !seen[path] {
			opts.LogFiles = append(opts.LogFiles, path)
			seen[path] = true
		}
	}

	// 1. Inspect writer if it is a file.
	if f, ok := w.(*os.File); ok && f != nil {
		addLog(f.Name())
	}

	// 2. Inspect command arguments for log flags (space-separated or colon-delimited).
	if c != nil {
		for i := 0; i < len(c.Args); i++ {
			arg := c.Args[i]

			// Check for colon-delimited log flags (e.g. /log:<path>, /l*v:<path>, -log:<path>, -l:<path>).
			if parts := strings.SplitN(arg, ":", 2); len(parts) == 2 {
				if isLogFlag(parts[0]) {
					addLog(strings.Trim(strings.TrimSpace(parts[1]), `"'`))
					continue
				}
			}

			// Check for space-separated log flags (e.g. /log <path>, /l*v <path>, -log <path>, -l <path>).
			if isLogFlag(arg) && i+1 < len(c.Args) && !isLogFlag(c.Args[i+1]) {
				addLog(strings.Trim(strings.TrimSpace(c.Args[i+1]), `"'`))
				i++
			}
		}
	}

	return opts
}

// Run runs a command.
// The process is successful if the exit code matches any of those provided or '0'.
// stdout and stderr are sent to the writer and to this process's stdout and
// stderr. While a progress spinner is active they are still shown as they are
// produced, a line at a time after clearing the spinner line; nothing is
// withheld or discarded.
func Run(c *exec.Cmd, ec []int, w io.Writer) error {
	return RunWithOptions(c, ec, supervisor.Options{}, w)
}

// RunWithOptions runs a command supervised by the supervisor package using custom options.
func RunWithOptions(c *exec.Cmd, ec []int, opts supervisor.Options, w io.Writer) error {
	opts = enrichOptions(c, opts, w)
	return checkExit(supervisor.Run(c, opts, w), ec)
}

// checkExit maps the result of supervisor.Run to the error returned by RunWithOptions.
// A nonzero exit status listed in ec is treated as success.
func checkExit(err error, ec []int) error {
	if err == nil {
		return nil
	}
	// Defense in depth: supervisor termination errors do not wrap an
	// *exec.ExitError today, so errors.As below would already fail for them.
	// This guard keeps a future termination error that does wrap one from
	// being accepted as success because its exit code is listed in ec.
	if errors.Is(err, supervisor.ErrTerminated) {
		return err
	}
	var e *exec.ExitError
	if !errors.As(err, &e) {
		return err
	}
	s, ok := e.Sys().(syscall.WaitStatus)
	if !ok {
		return err
	}
	if !slices.Contains(ec, s.ExitStatus()) {
		return fmt.Errorf("command exited with error code %v", s.ExitStatus())
	}
	return nil
}

// PackageInfo describes the name arch and version of a package.
type PackageInfo struct {
	Name, Arch, Ver string
}

func (pi PackageInfo) String() string {
	if pi.Arch != "" && pi.Ver != "" {
		return fmt.Sprintf("%s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	}
	if pi.Arch != "" {
		return fmt.Sprintf("%s.%s", pi.Name, pi.Arch)
	}
	return pi.Name
}

// PkgName returns the proper goo package name.
func (pi PackageInfo) PkgName() string {
	return fmt.Sprintf("%s.%s.%s.goo", pi.Name, pi.Arch, pi.Ver)
}

// PkgNameSplit returns the PackageInfo from a package name.
// If the package name does not contain arch or version an empty string
// will be returned.
func PkgNameSplit(pn string) PackageInfo {
	pi := strings.SplitN(strings.TrimSpace(pn), ".", 3)
	if len(pi) == 2 {
		return PackageInfo{pi[0], pi[1], ""}
	}
	if len(pi) == 3 {
		return PackageInfo{pi[0], pi[1], pi[2]}
	}
	return PackageInfo{pi[0], "", ""}
}

// Checksum retuns the SHA256 checksum of the provided reader.
func Checksum(r io.Reader) string {
	hash := sha256.New()
	io.Copy(hash, r)
	return hex.EncodeToString(hash.Sum(nil))
}

// ExtractPkgSpec pulls and unmarshals the package spec file from a
// reader.
func ExtractPkgSpec(r io.Reader) (*PkgSpec, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	return ReadPackageSpec(zr)
}

// SplitGCSUrl parses and splits a GCS URL returning if the URL belongs to a GCS object,
// and if so the bucket and object.
// Code modified from https://github.com/GoogleCloudPlatform/compute-image-tools/blob/master/daisy/storage.go
func SplitGCSUrl(p string) (bool, string, string) {
	bucket := `([a-z0-9][-_.a-z0-9]*)`
	object := `(/(?U)(.+)/*)?`
	bucketRegex := regexp.MustCompile(fmt.Sprintf(`^gs://%s/?$`, bucket))
	gsRegex := regexp.MustCompile(fmt.Sprintf(`^gs://%s%s$`, bucket, object))
	gsHTTPRegex1 := regexp.MustCompile(fmt.Sprintf(`^http[s]?://%s\.(?i:storage\.googleapis\.com)%s$`, bucket, object))
	gsHTTPRegex2 := regexp.MustCompile(fmt.Sprintf(`^http[s]?://(?i:storage\.cloud\.google\.com)/%s%s$`, bucket, object))
	gsHTTPRegex3 := regexp.MustCompile(fmt.Sprintf(`^http[s]?://(?i:(?:commondata)?storage\.googleapis\.com)/%s%s$`, bucket, object))

	for _, rgx := range []*regexp.Regexp{gsRegex, gsHTTPRegex1, gsHTTPRegex2, gsHTTPRegex3} {
		matches := rgx.FindStringSubmatch(p)
		if matches != nil {
			return true, matches[1], matches[3]
		}
	}

	matches := bucketRegex.FindStringSubmatch(p)
	if matches != nil {
		return true, matches[1], ""
	}

	return false, "", ""
}
