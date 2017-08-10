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

package client

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
)

const (
	cacheLife   = 1 * time.Minute
	port        = 56456
	proxyServer = ""
)

func init() {
	logger.Init("test", true, false, ioutil.Discard)
}

func TestAppend(t *testing.T) {
	s := &GooGetState{}
	s.Add(PackageState{SourceRepo: "test"})
	want := &GooGetState{PackageState{SourceRepo: "test"}}
	if !reflect.DeepEqual(want, s) {
		t.Errorf("Append did not produce expected result, want %+v, got: %+v", want, s)
	}
}

func TestRemove(t *testing.T) {
	s := &GooGetState{
		PackageState{PackageSpec: &goolib.PkgSpec{Name: "test"}},
		PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}},
	}
	if err := s.Remove(goolib.PackageInfo{Name: "test", Arch: "", Ver: ""}); err != nil {
		t.Errorf("error running Remove: %v", err)
	}
	if len(*s) != 1 {
		t.Errorf("Remove did not remove anything, want: len of 1, got: len of %d", len(*s))
	}
}

func TestRemoveNoMatch(t *testing.T) {
	s := &GooGetState{PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}}}
	if err := s.Remove(goolib.PackageInfo{Name: "test", Arch: "", Ver: ""}); err == nil {
		t.Error("did not get expected error when running Remove")
	}
}

func TestGetPackageState(t *testing.T) {
	want := PackageState{PackageSpec: &goolib.PkgSpec{Name: "test"}}
	s := &GooGetState{
		want,
		PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}},
	}
	got, err := s.GetPackageState(goolib.PackageInfo{Name: "test", Arch: "", Ver: ""})
	if err != nil {
		t.Errorf("error running GetPackageState: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetPackageState did not return expected result, want: %+v, got: %+v", got, want)
	}
}

func TestGetPackageStateNoMatch(t *testing.T) {
	s := &GooGetState{PackageState{PackageSpec: &goolib.PkgSpec{Name: "test2"}}}
	if _, err := s.GetPackageState(goolib.PackageInfo{Name: "test", Arch: "", Ver: ""}); err == nil {
		t.Error("did not get expected error when running GetPackageState")
	}
}

func TestWhatRepo(t *testing.T) {
	rm := RepoMap{
		"foo_repo": []goolib.RepoSpec{
			{
				PackageSpec: &goolib.PkgSpec{
					Name:    "foo_pkg",
					Version: "1.2.3@4",
					Arch:    "noarch",
				},
			},
		},
	}

	got, err := WhatRepo(goolib.PackageInfo{Name: "foo_pkg", Arch: "noarch", Ver: "1.2.3@4"}, rm)
	if err != nil {
		t.Fatalf("error running WhatRepo: %v", err)
	}
	if got != "foo_repo" {
		t.Errorf("returned repo does not match expected repo: got %q, want %q", got, "foo_repo")
	}
}

func TestFindRepoLatest(t *testing.T) {
	archs := []string{"noarch", "x86_64"}
	rm := RepoMap{
		"foo_repo": []goolib.RepoSpec{
			{
				PackageSpec: &goolib.PkgSpec{
					Name:    "foo_pkg",
					Version: "1.2.3@4",
					Arch:    "noarch",
				},
			},
			{
				PackageSpec: &goolib.PkgSpec{
					Name:    "foo_pkg",
					Version: "1.0.0@1",
					Arch:    "noarch",
				},
			},
			{
				PackageSpec: &goolib.PkgSpec{
					Name:    "bar_pkg",
					Version: "1.0.0@1",
					Arch:    "noarch",
				},
			},
		},
	}

	table := []struct {
		pkg   string
		arch  string
		wVer  string
		wArch string
		wRepo string
	}{
		{"foo_pkg", "noarch", "1.2.3@4", "noarch", "foo_repo"},
		{"foo_pkg", "", "1.2.3@4", "noarch", "foo_repo"},
	}
	for _, tt := range table {
		gotVer, gotRepo, gotArch, err := FindRepoLatest(goolib.PackageInfo{Name: tt.pkg, Arch: tt.arch, Ver: ""}, rm, archs)
		if err != nil {
			t.Fatalf("FindRepoLatest failed: %v", err)
		}
		if gotVer != tt.wVer {
			t.Errorf("FindRepoLatest for %q, %q returned version: %q, want %q", tt.pkg, tt.arch, gotVer, tt.wVer)
		}
		if gotArch != tt.wArch {
			t.Errorf("FindRepoLatest for %q, %q returned arch: %q, want %q", tt.pkg, tt.arch, gotArch, tt.wArch)
		}
		if gotRepo != tt.wRepo {
			t.Errorf("FindRepoLatest for %q, %q returned repo: %q, want %q", tt.pkg, tt.arch, gotRepo, tt.wRepo)
		}
	}

	werr := "no versions of package bar_pkg.x86_64 found in any repo"
	if _, _, _, err := FindRepoLatest(goolib.PackageInfo{Name: "bar_pkg", Arch: "x86_64", Ver: ""}, rm, archs); err.Error() != werr {
		t.Errorf("did not get expected error: got %q, want %q", err, werr)
	}
}

func TestUnmarshalRepoPackagesJSON(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	want := []goolib.RepoSpec{
		{Source: "foo"},
		{Source: "bar"},
	}
	j, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Error marshalling json: %v", err)
	}
	br := bytes.NewReader(j)

	http.HandleFunc("/test-repo/index", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.Copy(w, br)
	})

	go http.ListenAndServe(fmt.Sprintf(":%d", port), nil)

	got, err := unmarshalRepoPackages(fmt.Sprintf("http://localhost:%d/test-repo", port), tempDir, cacheLife, proxyServer)
	if err != nil {
		t.Fatalf("Error running unmarshalRepoPackages: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("unmarshalRepoPackages did not return expected content, got: %+v, want: %+v", got, want)
	}
}

func TestUnmarshalRepoPackagesGzip(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	want := []goolib.RepoSpec{
		{Source: "foo"},
		{Source: "bar"},
	}
	j, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Error marshalling json: %v", err)
	}

	var b bytes.Buffer
	gw := gzip.NewWriter(&b)
	if _, err := gw.Write(j); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("Error closing gzip writer: %v", err)
	}

	http.HandleFunc("/test-repo/index.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		io.Copy(w, &b)
	})

	go http.ListenAndServe(fmt.Sprintf(":%d", port), nil)

	got, err := unmarshalRepoPackages(fmt.Sprintf("http://localhost:%d/test-repo", port), tempDir, cacheLife, proxyServer)
	if err != nil {
		t.Fatalf("Error running unmarshalRepoPackages: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("unmarshalRepoPackages did not return expected content, got: %+v, want: %+v", got, want)
	}
}

func TestUnmarshalRepoPackagesCache(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	want := []goolib.RepoSpec{
		{Source: "foo"},
		{Source: "bar"},
	}
	j, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Error marshalling json: %v", err)
	}
	f, err := oswrap.Create(filepath.Join(tempDir, "test-repo.rs"))
	if err != nil {
		t.Fatalf("Error creating cache file: %v", err)
	}
	if _, err := f.Write(j); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Error closing file writer: %v", err)
	}

	// No http server as this should use the cached content.
	got, err := unmarshalRepoPackages("http://localhost/test-repo", tempDir, cacheLife, proxyServer)
	if err != nil {
		t.Fatalf("Error running unmarshalRepoPackages: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("unmarshalRepoPackages did not return expected content, got: %+v, want: %+v", got, want)
	}
}

func TestFindRepoSpec(t *testing.T) {
	want := goolib.RepoSpec{PackageSpec: &goolib.PkgSpec{Name: "test"}}
	rs := []goolib.RepoSpec{
		want,
		{PackageSpec: &goolib.PkgSpec{Name: "test2"}},
	}

	got, err := FindRepoSpec(goolib.PackageInfo{Name: "test", Arch: "", Ver: ""}, rs)
	if err != nil {
		t.Errorf("error running FindRepoSpec: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindRepoSpec did not return expected result, want: %+v, got: %+v", got, want)
	}
}

func TestFindRepoSpecNoMatch(t *testing.T) {
	rs := []goolib.RepoSpec{{PackageSpec: &goolib.PkgSpec{Name: "test2"}}}

	if _, err := FindRepoSpec(goolib.PackageInfo{Name: "test", Arch: "", Ver: ""}, rs); err == nil {
		t.Error("did not get expected error when running FindRepoSpec")
	}
}
