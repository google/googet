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
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/priority"
	"github.com/google/logger"
)

const (
	cacheLife   = 1 * time.Minute
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

func TestPackageMap(t *testing.T) {
	s := &GooGetState{
		PackageState{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.2.3@4", Arch: "noarch"}},
		PackageState{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "0.1.0@1", Arch: "noarch"}},
	}
	want := PackageMap{"foo.noarch": "1.2.3@4", "bar.noarch": "0.1.0@1"}
	got := s.PackageMap()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("PackageMap unexpected diff (-want +got):\n%v", diff)
	}
}

func TestWhatRepo(t *testing.T) {
	rm := RepoMap{
		"foo_repo": Repo{
			Packages: []goolib.RepoSpec{
				{
					PackageSpec: &goolib.PkgSpec{
						Name:    "foo_pkg",
						Version: "1.2.3@4",
						Arch:    "noarch",
					},
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
	for _, tt := range []struct {
		desc        string
		pi          goolib.PackageInfo
		archs       []string
		rm          RepoMap
		wantVersion string
		wantArch    string
		wantRepo    string
		wantErr     bool
	}{
		{
			desc:  "name and arch",
			pi:    goolib.PackageInfo{Name: "foo_pkg", Arch: "noarch"},
			archs: []string{"noarch", "x86_64", "arm64"},
			rm: RepoMap{
				"foo_repo": Repo{Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.2.3@4", Arch: "noarch"}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "x86_64"}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "3.0.0@1", Arch: "arm64"}},
					{PackageSpec: &goolib.PkgSpec{Name: "bar_pkg", Version: "2.3.0@1", Arch: "noarch"}},
				}},
			},
			wantVersion: "1.2.3@4",
			wantArch:    "noarch",
			wantRepo:    "foo_repo",
		},
		{
			desc:  "name only",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"noarch", "x86_64", "arm64"},
			rm: RepoMap{
				"foo_repo": Repo{Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.2.3@4", Arch: "noarch"}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "x86_64"}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "3.0.0@1", Arch: "arm64"}},
					{PackageSpec: &goolib.PkgSpec{Name: "bar_pkg", Version: "2.3.0@1", Arch: "noarch"}},
				}},
			},
			wantVersion: "1.2.3@4",
			wantArch:    "noarch",
			wantRepo:    "foo_repo",
		},
		{
			desc:  "specified arch not present",
			pi:    goolib.PackageInfo{Name: "foo_pkg", Arch: "x86_64"},
			archs: []string{"noarch", "x86_64", "arm64"},
			rm: RepoMap{
				"foo_repo": Repo{Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.2.3@4", Arch: "noarch"}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}},
					{PackageSpec: &goolib.PkgSpec{Name: "bar_pkg", Version: "2.3.0@1", Arch: "noarch"}},
				}},
			},
			wantErr: true,
		},
		{
			desc:  "multiple repos with same priority",
			pi:    goolib.PackageInfo{Name: "foo_pkg", Arch: "noarch"},
			archs: []string{"noarch", "x86_64", "arm64"},
			rm: RepoMap{
				"foo_repo": Repo{
					Priority: 500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.2.3@4", Arch: "noarch"}},
					},
				},
				"bar_repo": Repo{
					Priority: 500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.4.5@1", Arch: "noarch"}},
					},
				},
			},
			wantVersion: "2.4.5@1",
			wantArch:    "noarch",
			wantRepo:    "bar_repo",
		},
		{
			desc:  "multiple repos with different priority",
			pi:    goolib.PackageInfo{Name: "foo_pkg", Arch: "noarch"},
			archs: []string{"noarch", "x86_64", "arm64"},
			rm: RepoMap{
				"high_priority_repo": Repo{
					Priority: 1500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.2.3@4", Arch: "noarch"}},
					},
				},
				"low_priority_repo": Repo{
					Priority: 500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.4.5@1", Arch: "noarch"}},
					},
				},
			},
			wantVersion: "1.2.3@4",
			wantArch:    "noarch",
			wantRepo:    "high_priority_repo",
		},
	} {
		t.Run(tt.desc, func(t *testing.T) {
			gotSpec, gotRepo, gotArch, err := FindRepoLatest(tt.pi, tt.rm, tt.archs)
			if err != nil && !tt.wantErr {
				t.Fatalf("FindRepoLatest(%v, %v, %v) failed: %v", tt.pi, tt.rm, tt.archs, err)
			} else if err == nil && tt.wantErr {
				t.Fatalf("FindRepoLatest(%v, %v, %v) got nil error, wanted non-nil", tt.pi, tt.rm, tt.archs)
			}
			if err != nil {
				return
			}
			if gotSpec.Version != tt.wantVersion {
				t.Errorf("FindRepoLatest(%v, %v, %v) got version: %q, want %q", tt.pi, tt.rm, tt.archs, gotSpec.Version, tt.wantVersion)
			}
			if gotArch != tt.wantArch {
				t.Errorf("FindRepoLatest(%v, %v, %v) got arch: %q, want %q", tt.pi, tt.rm, tt.archs, gotArch, tt.wantArch)
			}
			if gotRepo != tt.wantRepo {
				t.Errorf("FindRepoLatest(%v, %v, %v) got repo: %q, want %q", tt.pi, tt.rm, tt.archs, gotRepo, tt.wantRepo)
			}
		})
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

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() == "/index" {
			w.Header().Set("Content-Type", "application/json")
			io.Copy(w, br)
		} else {
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	d, err := NewDownloader(proxyServer)
	if err != nil {
		t.Fatalf("NewDownloader(%s): %v", proxyServer, err)
	}
	got, err := d.unmarshalRepoPackages(context.Background(), ts.URL, tempDir, cacheLife)
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

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() == "/index.gz" {
			w.Header().Set("Content-Type", "application/gzip")
			io.Copy(w, &b)
		} else {
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	d, err := NewDownloader(proxyServer)
	if err != nil {
		t.Fatalf("NewDownloader(%s): %v", proxyServer, err)
	}
	got, err := d.unmarshalRepoPackages(context.Background(), ts.URL, tempDir, cacheLife)
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
	url := "http://localhost/test-repo"
	f, err := oswrap.Create(filepath.Join(tempDir, fmt.Sprintf("%x.rs", sha256.Sum256([]byte(url)))))
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
	d, err := NewDownloader(proxyServer)
	if err != nil {
		t.Fatalf("NewDownloader(%s): %v", proxyServer, err)
	}
	got, err := d.unmarshalRepoPackages(context.Background(), url, tempDir, cacheLife)
	if err != nil {
		t.Fatalf("Error running unmarshalRepoPackages: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("unmarshalRepoPackages did not return expected content, got: %+v, want: %+v", got, want)
	}
}

func TestFindRepoSpec(t *testing.T) {
	want := goolib.RepoSpec{PackageSpec: &goolib.PkgSpec{Name: "test"}}
	repo := Repo{Packages: []goolib.RepoSpec{
		want,
		{PackageSpec: &goolib.PkgSpec{Name: "test2"}},
	}}

	got, err := FindRepoSpec(goolib.PackageInfo{Name: "test", Arch: "", Ver: ""}, repo)
	if err != nil {
		t.Errorf("error running FindRepoSpec: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindRepoSpec did not return expected result, want: %+v, got: %+v", got, want)
	}
}

func TestFindRepoSpecNoMatch(t *testing.T) {
	repo := Repo{Packages: []goolib.RepoSpec{{PackageSpec: &goolib.PkgSpec{Name: "test2"}}}}

	if _, err := FindRepoSpec(goolib.PackageInfo{Name: "test", Arch: "", Ver: ""}, repo); err == nil {
		t.Error("did not get expected error when running FindRepoSpec")
	}
}

func TestFindRepoLatest_Provides(t *testing.T) {
	rm := RepoMap{
		"repo1": Repo{
			Priority: priority.Value(500),
			Packages: []goolib.RepoSpec{
				{
					PackageSpec: &goolib.PkgSpec{
						Name:    "real_pkg",
						Version: "2.0.0",
						Arch:    "noarch",
						Provides: []string{
							"virtual_pkg",
							"virtual_versioned=1.0.0",
						},
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{
						Name:    "real_pkg_old",
						Version: "1.0.0",
						Arch:    "noarch",
						Provides: []string{
							"virtual_pkg",
						},
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{
						Name:    "other_pkg",
						Version: "1.0.0",
						Arch:    "noarch",
					},
				},
			},
		},
	}

	tests := []struct {
		name      string
		pi        goolib.PackageInfo
		wantName  string
		wantVer   string
		wantError bool
	}{
		{
			name:     "Direct match",
			pi:       goolib.PackageInfo{Name: "other_pkg", Arch: "noarch"},
			wantName: "other_pkg",
			wantVer:  "1.0.0",
		},
		{
			name:     "Provider match unversioned",
			pi:       goolib.PackageInfo{Name: "virtual_pkg", Arch: "noarch"},
			wantName: "real_pkg",
			wantVer:  "2.0.0", // latest real_pkg
		},
		{
			name:     "Provider match matched version",
			pi:       goolib.PackageInfo{Name: "virtual_versioned", Ver: "1.0.0", Arch: "noarch"},
			wantName: "real_pkg",
			wantVer:  "2.0.0",
		},
		{
			name:      "Provider match unsatisfied version",
			pi:        goolib.PackageInfo{Name: "virtual_versioned", Ver: "2.0.0", Arch: "noarch"},
			wantError: true,
		},
		{
			name:      "No match",
			pi:        goolib.PackageInfo{Name: "missing_pkg", Arch: "noarch"},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, _, _, err := FindRepoLatest(tt.pi, rm, []string{"noarch"})
			if tt.wantError {
				if err == nil {
					t.Errorf("FindRepoLatest(%v) wanted error, got nil", tt.pi)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindRepoLatest(%v) unexpected error: %v", tt.pi, err)
			}
			if spec.Name != tt.wantName {
				t.Errorf("FindRepoLatest(%v) name = %q, want %q", tt.pi, spec.Name, tt.wantName)
			}
			if spec.Version != tt.wantVer { // Simplified check, assumes simple version strings in test
				t.Errorf("FindRepoLatest(%v) version = %q, want %q", tt.pi, spec.Version, tt.wantVer)
			}
		})
	}
}

func TestFindRepoLatest_Priority(t *testing.T) {
	// Setup repo with both direct match and provider.
	// direct match: version 1.0.0
	// provider: version 2.0.0 (provides it)
	// direct match should win despite lower version.

	rm := RepoMap{
		"repo1": Repo{
			Priority: priority.Value(500),
			Packages: []goolib.RepoSpec{
				{
					PackageSpec: &goolib.PkgSpec{
						Name:    "real_pkg",
						Version: "1.0.0",
						Arch:    "noarch",
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{
						Name:    "provider_pkg",
						Version: "2.0.0",
						Arch:    "noarch",
						Provides: []string{
							"real_pkg",
						},
					},
				},
			},
		},
	}

	pi := goolib.PackageInfo{Name: "real_pkg", Arch: "noarch"}
	spec, _, _, err := FindRepoLatest(pi, rm, []string{"noarch"})
	if err != nil {
		t.Fatalf("FindRepoLatest failed: %v", err)
	}

	if spec.Name != "real_pkg" {
		t.Errorf("Expected direct match 'real_pkg', got '%s'", spec.Name)
	}
	if spec.Version != "1.0.0" {
		t.Errorf("Expected version '1.0.0', got '%s'", spec.Version)
	}
}
