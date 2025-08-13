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
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/settings"
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

func TestCleanPackages(t *testing.T) {
	rootDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(rootDir)
	settings.Initialize(rootDir, false)

	cache := settings.CacheDir()
	wantFile := filepath.Join(cache, "want")
	notWantFile := filepath.Join(cache, "notWant")

	if err := oswrap.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(wantFile, nil, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(notWantFile, nil, 0700); err != nil {
		t.Fatal(err)
	}

	state := client.GooGetState{
		{LocalPath: wantFile, PackageSpec: &goolib.PkgSpec{Name: "want"}},
		{LocalPath: notWantFile, PackageSpec: &goolib.PkgSpec{Name: "notWant"}},
	}
	cleanInstalled(state, map[string]bool{"notWant": true})

	if _, err := oswrap.Stat(wantFile); err != nil {
		t.Errorf("cleanPackages removed wantDir, Stat err: %v", err)
	}

	if _, err := oswrap.Stat(notWantFile); err == nil {
		t.Errorf("cleanPackages did not remove notWantDir")
	}
}

func TestUpdates(t *testing.T) {
	for _, tc := range []struct {
		name string
		pm   client.PackageMap
		rm   client.RepoMap
		want []goolib.PackageInfo
	}{
		{
			name: "upgrade to later version",
			pm: client.PackageMap{
				"foo.x86_32": "1.0",
				"bar.x86_32": "2.0",
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: 500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
			},
			want: []goolib.PackageInfo{{Name: "foo", Arch: "x86_32", Ver: "2.0"}},
		},
		{
			name: "rollback to earlier version",
			pm: client.PackageMap{
				"foo.x86_32": "2.0",
				"bar.x86_32": "2.0",
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: 500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
				"rollback": client.Repo{
					Priority: 1500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			want: []goolib.PackageInfo{{Name: "foo", Arch: "x86_32", Ver: "1.0"}},
		},
		{
			name: "no change if rollback version already installed",
			pm: client.PackageMap{
				"foo.x86_32": "1.0",
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: 500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
				"rollback": client.Repo{
					Priority: 1500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pi := updates(tc.pm, tc.rm)
			if diff := cmp.Diff(tc.want, pi); diff != "" {
				t.Errorf("update(%v, %v) got unexpected diff (-want +got):\n%v", tc.pm, tc.rm, diff)
			}
		})
	}
}

func TestObtainLock(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	lockFile := settings.LockFile()
	cleanup, err := obtainLock(lockFile)
	if err != nil {
		t.Fatalf("obtainLock: %v", err)
	}
	if cleanup == nil {
		t.Fatalf("obtainLock got nil cleanup, want non-nil")
	}
	if _, err := os.Stat(lockFile); errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("os.Stat(%v): lockfile does not exist", lockFile)
	}
	cleanup()
	if _, err := os.Stat(lockFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("os.Stat(%v): lockfile still exists after cleanup", lockFile)
	}
}
