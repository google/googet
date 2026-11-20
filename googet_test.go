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
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeLogs creates each named file under dir with its name as content, so
// tests can tell after rotation which file ended up where. The live log gets
// live bytes of padding after its name.
func writeLogs(t *testing.T, dir string, live int64, names ...string) {
	t.Helper()
	for _, n := range names {
		content := []byte(n)
		if n == "googet.log" {
			content = append(content, make([]byte, live)...)
		}
		if err := os.WriteFile(filepath.Join(dir, n), content, 0644); err != nil {
			t.Fatalf("writing %s: %v", n, err)
		}
	}
}

// logState maps each file in dir to the name it was created with, as written
// by writeLogs.
func logState(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	got := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		got[e.Name()] = strings.TrimRight(string(b), "\x00")
	}
	return got
}

// logNames lists the names in dir.
func logNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// spyRotate wraps rotateRename and rotateRemove for the rest of the test. It
// records every call and fails the ones fail selects. fail gets the base
// names of the source and destination; dst is empty for removes.
func spyRotate(t *testing.T, fail func(op, src, dst string) bool) *[]string {
	t.Helper()
	oldRename, oldRemove := rotateRename, rotateRemove
	t.Cleanup(func() { rotateRename, rotateRemove = oldRename, oldRemove })
	var calls []string
	rotateRename = func(src, dst string) error {
		calls = append(calls, "rename "+filepath.Base(src)+" "+filepath.Base(dst))
		if fail != nil && fail("rename", filepath.Base(src), filepath.Base(dst)) {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errors.New("file is locked")}
		}
		return oldRename(src, dst)
	}
	rotateRemove = func(name string) error {
		calls = append(calls, "remove "+filepath.Base(name))
		if fail != nil && fail("remove", filepath.Base(name), "") {
			return &os.PathError{Op: "remove", Path: name, Err: errors.New("file is locked")}
		}
		return oldRemove(name)
	}
	return &calls
}

// lockedOnWindows returns a fail function for spyRotate that models a file
// locked on Windows: it can't be deleted, moved, or replaced by a rename.
func lockedOnWindows(locked string) func(op, src, dst string) bool {
	return func(op, src, dst string) bool { return src == locked || dst == locked }
}

func TestLogGeneration(t *testing.T) {
	for n, want := range map[int]string{0: "g.log.old", 1: "g.log.old", 2: "g.log.old.2", 5: "g.log.old.5"} {
		if got := logGeneration("g.log", n); got != want {
			t.Errorf("logGeneration(g.log, %d) = %q, want %q", n, got, want)
		}
	}
}

func TestRotateLog(t *testing.T) {
	const maxSize = 1024
	for _, tc := range []struct {
		desc  string
		live  int64
		files []string
		want  map[string]string // File name to original name.
	}{
		{
			desc:  "below the cap",
			live:  maxSize - 20,
			files: []string{"googet.log", "googet.log.old"},
			want:  map[string]string{"googet.log": "googet.log", "googet.log.old": "googet.log.old"},
		},
		{
			desc:  "no old logs",
			live:  maxSize,
			files: []string{"googet.log"},
			want:  map[string]string{"googet.log.old": "googet.log"},
		},
		{
			// A legacy .old written by an older googet becomes generation 2.
			desc:  "legacy old log only",
			live:  maxSize,
			files: []string{"googet.log", "googet.log.old"},
			want:  map[string]string{"googet.log.old": "googet.log", "googet.log.old.2": "googet.log.old"},
		},
		{
			desc:  "all generations present",
			live:  maxSize,
			files: []string{"googet.log", "googet.log.old", "googet.log.old.2", "googet.log.old.3"},
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.3": "googet.log.old.2",
			},
		},
		{
			// A missing generation is skipped without a warning.
			desc:  "missing middle generation",
			live:  maxSize,
			files: []string{"googet.log", "googet.log.old", "googet.log.old.3"},
			want:  map[string]string{"googet.log.old": "googet.log", "googet.log.old.2": "googet.log.old"},
		},
		{
			// Generations beyond keep, such as after keep is lowered, are
			// left alone rather than counted.
			desc:  "generation beyond keep",
			live:  maxSize,
			files: []string{"googet.log", "googet.log.old", "googet.log.old.4"},
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.4": "googet.log.old.4",
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			calls := spyRotate(t, nil)
			dir := t.TempDir()
			writeLogs(t, dir, tc.live, tc.files...)
			if errs := rotateLog(filepath.Join(dir, "googet.log"), maxSize, 3); len(errs) != 0 {
				t.Errorf("rotateLog() = %v, want no warnings", errs)
			}
			if got := logState(t, dir); !maps.Equal(got, tc.want) {
				t.Errorf("after rotateLog() files = %v, want %v", got, tc.want)
			}
			if tc.live < maxSize && len(*calls) != 0 {
				t.Errorf("rotateLog() below the cap made calls %q, want none", *calls)
			}
		})
	}
}

// TestRotateLogBestEffort checks that a file that can't be moved or deleted
// produces a warning, never an error that stops googet.
func TestRotateLogBestEffort(t *testing.T) {
	const maxSize = 1024
	all := []string{"googet.log", "googet.log.old", "googet.log.old.2", "googet.log.old.3"}
	for _, tc := range []struct {
		desc      string
		fail      func(op, src, dst string) bool
		want      map[string]string
		wantWarns int
	}{
		{
			// Only renames from .old.2 fail, as on POSIX, where a locked file
			// can still be renamed over. .old moves onto .old.2 and replaces
			// it, and the live log still rotates.
			desc: "older generation can't be moved",
			fail: func(op, src, dst string) bool { return op == "rename" && src == "googet.log.old.2" },
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
			},
			wantWarns: 1,
		},
		{
			// On Windows a locked .old.2 can't be moved or renamed over, so
			// .old stays in place and the live log isn't rotated. The oldest
			// generation has already been deleted by then.
			desc: "older generation locked on Windows",
			fail: lockedOnWindows("googet.log.old.2"),
			want: map[string]string{
				"googet.log":       "googet.log",
				"googet.log.old":   "googet.log.old",
				"googet.log.old.2": "googet.log.old.2",
			},
			wantWarns: 3,
		},
		{
			// On Windows a locked oldest generation can't be deleted or
			// renamed over, so .old.2 stays where it is and .old replaces it.
			// The locked file survives and the live log still rotates.
			desc: "oldest generation locked on Windows",
			fail: lockedOnWindows("googet.log.old.3"),
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.3": "googet.log.old.3",
			},
			wantWarns: 2,
		},
		{
			desc: "oldest generation can't be deleted",
			fail: func(op, src, dst string) bool { return op == "remove" && src == "googet.log.old.3" },
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.3": "googet.log.old.2",
			},
			wantWarns: 1,
		},
		{
			// .old is never overwritten, so the live log stays in place and
			// googet keeps appending to it.
			desc: "newest generation locked",
			fail: func(op, src, dst string) bool { return op == "rename" && src == "googet.log.old" },
			want: map[string]string{
				"googet.log":       "googet.log",
				"googet.log.old":   "googet.log.old",
				"googet.log.old.3": "googet.log.old.2",
			},
			wantWarns: 2,
		},
		{
			desc: "live log locked",
			fail: func(op, src, dst string) bool { return op == "rename" && src == "googet.log" },
			want: map[string]string{
				"googet.log":       "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.3": "googet.log.old.2",
			},
			wantWarns: 1,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			spyRotate(t, tc.fail)
			dir := t.TempDir()
			writeLogs(t, dir, maxSize, all...)
			errs := rotateLog(filepath.Join(dir, "googet.log"), maxSize, 3)
			if len(errs) != tc.wantWarns {
				t.Errorf("rotateLog() = %d warnings %v, want %d", len(errs), errs, tc.wantWarns)
			}
			if got := logState(t, dir); !maps.Equal(got, tc.want) {
				t.Errorf("after rotateLog() files = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("generation is a non-empty directory", func(t *testing.T) {
		dir := t.TempDir()
		writeLogs(t, dir, maxSize, "googet.log", "googet.log.old")
		blocker := filepath.Join(dir, "googet.log.old.3", "x")
		if err := os.MkdirAll(blocker, 0755); err != nil {
			t.Fatal(err)
		}
		errs := rotateLog(filepath.Join(dir, "googet.log"), maxSize, 3)
		if len(errs) == 0 {
			t.Error("rotateLog() = no warnings, want one for the undeletable generation")
		}
		got := logNames(t, dir)
		if !slices.Contains(got, "googet.log.old") || slices.Contains(got, "googet.log") {
			t.Errorf("after rotateLog() files = %v, want the live log rotated to googet.log.old", got)
		}
	})
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
