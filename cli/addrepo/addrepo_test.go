/*
Copyright 2026 Google Inc. All Rights Reserved.
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

package addrepo

import (
	"context"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() {
	logger.Init("test", true, false, io.Discard)
}

const testURL = "https://example.com/googet/repo"

// runAddRepo runs addrepo with args against a googet root inside a fresh
// parent directory, and returns the exit status, the parent directory and the
// repos directory.
func runAddRepo(t *testing.T, args ...string) (status subcommands.ExitStatus, parent, repoDir string) {
	t.Helper()
	return runAddRepoWithSetup(t, nil, args...)
}

// runAddRepoWithSetup is like runAddRepo, but calls setup, if not nil, with
// the parent and repos directories before running addrepo.
func runAddRepoWithSetup(t *testing.T, setup func(parent, repoDir string), args ...string) (status subcommands.ExitStatus, parent, repoDir string) {
	t.Helper()
	parent = t.TempDir()
	oldRoot := settings.RootDir
	t.Cleanup(func() { settings.RootDir = oldRoot })
	settings.RootDir = filepath.Join(parent, "root")
	repoDir = settings.RepoDir()
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("creating %q: %v", repoDir, err)
	}
	if setup != nil {
		setup(parent, repoDir)
	}
	cmd := &addRepoCmd{}
	fs := flag.NewFlagSet("addrepo", flag.ContinueOnError)
	cmd.SetFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %q: %v", args, err)
	}
	return cmd.Execute(context.Background(), fs), parent, repoDir
}

// filesUnder returns the paths of all regular files under dir, relative to
// dir and with forward slashes.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", dir, err)
	}
	return files
}

func TestAddRepoRejectsFileOutsideRepoDir(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "abs.repo")
	for _, tc := range []struct {
		desc string
		args []string
	}{
		{desc: "parent dir in -file", args: []string{"-file", "../evil.repo", "foo", testURL}},
		{desc: "grandparent dir in -file", args: []string{"-file", "../../evil.repo", "foo", testURL}},
		{desc: "subdir in -file", args: []string{"-file", "sub/evil.repo", "foo", testURL}},
		{desc: "backslash in -file", args: []string{"-file", `..\evil.repo`, "foo", testURL}},
		{desc: "colon in -file", args: []string{"-file", "c:evil.repo", "foo", testURL}},
		{desc: "absolute -file", args: []string{"-file", abs, "foo", testURL}},
		{desc: "empty name in -file", args: []string{"-file", ".repo", "foo", testURL}},
		{desc: "-file without .repo", args: []string{"-file", "evil.txt", "foo", testURL}},
		{desc: "parent dir in repo name", args: []string{"../evil", testURL}},
		{desc: "slash in repo name", args: []string{"sub/evil", testURL}},
		{desc: "backslash in repo name", args: []string{`sub\evil`, testURL}},
		{desc: "colon in repo name", args: []string{"c:evil", testURL}},
		{desc: "empty repo name", args: []string{"", testURL}},
		{desc: "reserved repo name", args: []string{"NUL", testURL}},
		{desc: "lowercase reserved repo name", args: []string{"aux", testURL}},
		{desc: "reserved -file", args: []string{"-file", "CON.repo", "foo", testURL}},
		{desc: "reserved COM port in -file", args: []string{"-file", "COM1.repo", "foo", testURL}},
		{desc: "reserved LPT port in -file", args: []string{"-file", "lpt9.repo", "foo", testURL}},
		{desc: "reserved superscript COM port in -file", args: []string{"-file", "COM\u00b9.repo", "foo", testURL}},
		{desc: "reserved console in -file", args: []string{"-file", "conin$.repo", "foo", testURL}},
		{desc: "reserved name with extra extension in -file", args: []string{"-file", "nul.txt.repo", "foo", testURL}},
		{desc: "reserved name with trailing space in -file", args: []string{"-file", "PRN .repo", "foo", testURL}},
		{desc: "dry run with parent dir in -file", args: []string{"-dry_run", "-file", "../evil.repo", "foo", testURL}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			status, parent, _ := runAddRepo(t, tc.args...)
			if status != subcommands.ExitUsageError {
				t.Errorf("addrepo %q = %v, want %v", tc.args, status, subcommands.ExitUsageError)
			}
			if files := filesUnder(t, parent); len(files) != 0 {
				t.Errorf("addrepo %q wrote %q, want nothing written", tc.args, files)
			}
			if _, err := os.Lstat(abs); !os.IsNotExist(err) {
				t.Errorf("addrepo %q wrote %q (Lstat error: %v)", tc.args, abs, err)
			}
		})
	}
}

func TestAddRepoWritesInRepoDir(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		args     []string
		wantFile string
	}{
		{desc: "name from repo name", args: []string{"foo", testURL}, wantFile: "foo.repo"},
		{desc: "name from -file", args: []string{"-file", "my.repo", "foo", testURL}, wantFile: "my.repo"},
		{desc: "dotted repo name", args: []string{"foo.bar", testURL}, wantFile: "foo.bar.repo"},
		{desc: "reserved name prefix", args: []string{"console", testURL}, wantFile: "console.repo"},
		{desc: "COM without port number", args: []string{"-file", "com.repo", "foo", testURL}, wantFile: "com.repo"},
		{desc: "COM with two-digit port number", args: []string{"-file", "COM10.repo", "foo", testURL}, wantFile: "COM10.repo"},
		{desc: "reserved name after first dot", args: []string{"foo.nul", testURL}, wantFile: "foo.nul.repo"},
		{desc: "non-ASCII letter folding to reserved name", args: []string{"-file", "con\u0131n$.repo", "foo", testURL}, wantFile: "con\u0131n$.repo"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			status, parent, repoDir := runAddRepo(t, tc.args...)
			if status != subcommands.ExitSuccess {
				t.Fatalf("addrepo %q = %v, want %v", tc.args, status, subcommands.ExitSuccess)
			}
			want := []string{"root/repos/" + tc.wantFile}
			if files := filesUnder(t, parent); !slices.Equal(files, want) {
				t.Errorf("addrepo %q wrote %q, want %q", tc.args, files, want)
			}
			b, err := os.ReadFile(filepath.Join(repoDir, tc.wantFile))
			if err != nil {
				t.Fatalf("reading repo file: %v", err)
			}
			if !strings.Contains(string(b), testURL) {
				t.Errorf("repo file %q = %q, want it to contain %q", tc.wantFile, b, testURL)
			}
		})
	}
}

func TestAddRepoRejectsNonRegularRepoFile(t *testing.T) {
	for _, tc := range []struct {
		desc  string
		setup func(t *testing.T, outside, link string)
	}{
		{
			desc: "symlink",
			setup: func(t *testing.T, outside, link string) {
				if err := os.WriteFile(outside, []byte("# outside\n"), 0644); err != nil {
					t.Fatalf("writing %q: %v", outside, err)
				}
				symlinkOrSkip(t, outside, link)
			},
		},
		{
			desc: "dangling symlink",
			setup: func(t *testing.T, outside, link string) {
				symlinkOrSkip(t, outside, link)
			},
		},
		{
			desc: "directory",
			setup: func(t *testing.T, _, link string) {
				if err := os.Mkdir(link, 0755); err != nil {
					t.Fatalf("creating %q: %v", link, err)
				}
			},
		},
	} {
		for _, run := range []struct {
			desc string
			args []string
		}{
			{desc: "name from repo name", args: []string{"link", testURL}},
			{desc: "name from -file", args: []string{"-file", "link.repo", "foo", testURL}},
			{desc: "dry run", args: []string{"-dry_run", "-file", "link.repo", "foo", testURL}},
		} {
			args := run.args
			t.Run(tc.desc+" "+run.desc, func(t *testing.T) {
				var outside string
				var before []byte
				setup := func(parent, repoDir string) {
					outside = filepath.Join(parent, "outside.repo")
					tc.setup(t, outside, filepath.Join(repoDir, "link.repo"))
					before, _ = os.ReadFile(outside)
				}
				status, _, _ := runAddRepoWithSetup(t, setup, args...)
				if status != subcommands.ExitUsageError {
					t.Errorf("addrepo %q = %v, want %v", args, status, subcommands.ExitUsageError)
				}
				after, err := os.ReadFile(outside)
				if before == nil && !os.IsNotExist(err) {
					t.Errorf("addrepo %q created %q (read error: %v)", args, outside, err)
				}
				if before != nil && string(after) != string(before) {
					t.Errorf("addrepo %q changed %q to %q, want %q", args, outside, after, before)
				}
			})
		}
	}
}

// symlinkOrSkip creates link pointing to target, and skips the test if the
// platform or the current user cannot create symlinks.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink %q: %v", link, err)
	}
}
