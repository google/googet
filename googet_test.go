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
	"io/ioutil"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
)

func TestRepoList(t *testing.T) {
	testRepo := "https://foo.com/googet/bar"
	testHTTPRepo := "http://foo.com/googet/bar"

	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	testFile := filepath.Join(tempDir, "test.repo")

	repoTests := []struct {
		content        []byte
		want           []string
		allowUnsafeURL bool
	}{
		{[]byte("\n"), nil, false},
		{[]byte("# This is just a comment"), nil, false},
		{[]byte("url: " + testRepo), []string{testRepo}, false},
		{[]byte("\n # Comment\nurl: " + testRepo), []string{testRepo}, false},
		{[]byte("- url: " + testRepo), []string{testRepo}, false},
		// The HTTP repo should be dropped.
		{[]byte("- url: " + testHTTPRepo), nil, false},
		// The HTTP repo should not be dropped.
		{[]byte("- url: " + testHTTPRepo), []string{testHTTPRepo}, true},
		{[]byte("- URL: " + testRepo), []string{testRepo}, false},
		// The HTTP repo should be dropped.
		{[]byte("- url: " + testRepo + "\n\n- URL: " + testHTTPRepo), []string{testRepo}, false},
		// The HTTP repo should not be dropped.
		{[]byte("- url: " + testRepo + "\n\n- URL: " + testHTTPRepo), []string{testRepo, testHTTPRepo}, true},
		{[]byte("- url: " + testRepo + "\n\n- URL: " + testRepo), []string{testRepo, testRepo}, false},
		{[]byte("- url: " + testRepo + "\n\n- url: " + testRepo), []string{testRepo, testRepo}, false},
		// Should contain oauth- prefix
		{[]byte("- url: " + testRepo + "\n  useoauth: true"), []string{"oauth-" + testRepo}, false},
		// Should not contain oauth- prefix
		{[]byte("- url: " + testRepo + "\n  useoauth: false"), []string{testRepo}, false},
	}

	for i, tt := range repoTests {
		if err := ioutil.WriteFile(testFile, tt.content, 0660); err != nil {
			t.Fatalf("error writing repo: %v", err)
		}
		allowUnsafeURL = tt.allowUnsafeURL
		got, err := repoList(tempDir)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("test case %d: returned repo does not match expected repo: got %q, want %q", i+1, got, tt.want)
		}
	}
}

func TestInstalledPackages(t *testing.T) {
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "foo",
				Version: "1.2.3@4",
				Arch:    "noarch",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "bar",
				Version: "0.1.0@1",
				Arch:    "noarch",
			},
		},
	}

	want := packageMap{"foo.noarch": "1.2.3@4", "bar.noarch": "0.1.0@1"}
	got := installedPackages(state)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("returned map does not match expected map: got %v, want %v", got, want)
	}
}

func TestReadConf(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	confPath := filepath.Join(tempDir, "test.conf")
	f, err := oswrap.Create(confPath)
	if err != nil {
		t.Fatalf("error creating conf file: %v", err)
	}

	content := []byte("archs: [noarch, x86_64]\ncachelife: 10m\nallowunsafeurl: true")
	if _, err := f.Write(content); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error closing conf file: %v", err)
	}

	readConf(confPath)

	ea := []string{"noarch", "x86_64"}
	if !reflect.DeepEqual(archs, ea) {
		t.Errorf("readConf did not create expected arch list, want: %s, got: %s", ea, archs)
	}

	ecl := time.Duration(10 * time.Minute)
	if cacheLife != ecl {
		t.Errorf("readConf did not create expected cacheLife, want: %s, got: %s", ecl, cacheLife)
	}

	if allowUnsafeURL != true {
		t.Error("readConf did not set allowunsafeurl to true")
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

func TestWriteReadState(t *testing.T) {
	want := &client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test"}},
	}

	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	sf := filepath.Join(tempDir, "test.state")

	if err := writeState(want, sf); err != nil {
		t.Errorf("error running writeState: %v", err)
	}

	got, err := readState(sf)
	if err != nil {
		t.Errorf("error running readState: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("did not get expected state, got: %+v, want %+v", got, want)
	}
}

func TestReadStateRecovery(t *testing.T) {
	original := &client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test.org"}},
	}

	overwrite := &client.GooGetState{
		client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "test.new"}},
	}

	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	const (
		deleteState int = iota
		corruptState
	)

	table := []struct {
		name       string
		disruption int
	}{
		{"test-deleted.state", deleteState},
		{"test-corrupted.state", corruptState},
	}

	for _, tt := range table {
		sf := filepath.Join(tempDir, tt.name)

		if err := writeState(original, sf); err != nil {
			t.Errorf("error running writeState: %v", err)
		}

		if err := writeState(overwrite, sf); err != nil {
			t.Errorf("error running writeState second time: %v", err)
		}

		got, err := readState(sf)
		if err != nil {
			t.Errorf("error running readState: %v", err)
		}

		if !reflect.DeepEqual(got, overwrite) {
			t.Errorf("did not get expected state after overwrite, got: %+v, want %+v", got, overwrite)
		}

		switch tt.disruption {
		case deleteState:
			if err := oswrap.Remove(sf); err != nil {
				t.Errorf("error deleting state: %v", err)
			}
		case corruptState:
			if err := ioutil.WriteFile(sf, []byte{0, 0, 0, 0}, 0664); err != nil {
				t.Errorf("error corrupting state: %v", err)
			}
		}

		got, err = readState(sf)
		if err != nil {
			t.Errorf("error running readState after corruption of active state: %v", err)
		}

		if !reflect.DeepEqual(got, original) {
			t.Errorf("did not get expected state after corruption, got: %+v, want %+v", got, original)
		}
	}
}

func TestCleanOld(t *testing.T) {
	var err error
	rootDir, err = ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(rootDir)

	wantFile := filepath.Join(rootDir, cacheDir, "want.goo")
	notWantDir := filepath.Join(rootDir, cacheDir, "notWant")
	notWantFile := filepath.Join(rootDir, cacheDir, "notWant.goo")

	if err := oswrap.MkdirAll(notWantDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(notWantFile, nil, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(wantFile, nil, 0700); err != nil {
		t.Fatal(err)
	}

	state := &client.GooGetState{
		{
			LocalPath: wantFile,
		},
	}

	if err := writeState(state, filepath.Join(rootDir, stateFile)); err != nil {
		t.Fatalf("error running writeState: %v", err)
	}

	cleanOld()

	if _, err := oswrap.Stat(wantFile); err != nil {
		t.Errorf("cleanOld removed wantDir, Stat err: %v", err)
	}

	if _, err := oswrap.Stat(notWantDir); err == nil {
		t.Errorf("cleanOld did not remove notWantDir")
	}

	if _, err := oswrap.Stat(notWantFile); err == nil {
		t.Errorf("cleanOld did not remove notWantFile")
	}
}

func TestCleanPackages(t *testing.T) {
	var err error
	rootDir, err = ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(rootDir)

	wantFile := filepath.Join(rootDir, cacheDir, "want")
	notWantFile := filepath.Join(rootDir, cacheDir, "notWant")

	if err := oswrap.MkdirAll(filepath.Join(rootDir, cacheDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(wantFile, nil, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(notWantFile, nil, 0700); err != nil {
		t.Fatal(err)
	}

	state := &client.GooGetState{
		{
			LocalPath: wantFile,
			PackageSpec: &goolib.PkgSpec{
				Name: "want",
			},
		},
		{
			LocalPath: notWantFile,
			PackageSpec: &goolib.PkgSpec{
				Name: "notWant",
			},
		},
	}

	if err := writeState(state, filepath.Join(rootDir, stateFile)); err != nil {
		t.Fatalf("error running writeState: %v", err)
	}

	cleanPackages([]string{"notWant"})

	if _, err := oswrap.Stat(wantFile); err != nil {
		t.Errorf("cleanPackages removed wantDir, Stat err: %v", err)
	}

	if _, err := oswrap.Stat(notWantFile); err == nil {
		t.Errorf("cleanPackages did not remove notWantDir")
	}
}
