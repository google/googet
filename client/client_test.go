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
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
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
			wantVersion: "3.0.0@1",
			wantArch:    "arm64",
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
		{
			desc:  "version priority over arch",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"noarch", "x86_64"},
			rm: RepoMap{
				"foo_repo": Repo{Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "x86_64"}},
				}},
			},
			wantVersion: "2.0.0@1",
			wantArch:    "x86_64",
			wantRepo:    "foo_repo",
		},
		{
			desc:  "priority wins over version",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"noarch"},
			rm: RepoMap{
				"high_pri": Repo{
					Priority: 1000,
					Packages: []goolib.RepoSpec{{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}}},
				},
				"low_pri": Repo{
					Priority: 500,
					Packages: []goolib.RepoSpec{{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "noarch"}}},
				},
			},
			wantVersion: "1.0.0@1",
			wantArch:    "noarch",
			wantRepo:    "high_pri",
		},
		{
			desc:  "version wins over arch",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"x86_64", "x86_32"},
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_64"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "x86_32"}},
					},
				},
			},
			wantVersion: "2.0.0@1",
			wantArch:    "x86_32",
			wantRepo:    "repo",
		},
		{
			desc:  "arch wins tie",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"x86_64", "x86_32"},
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_64"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_32"}},
					},
				},
			},
			wantVersion: "1.0.0@1",
			wantArch:    "x86_64",
			wantRepo:    "repo",
		},
		{
			desc:  "cross arch upgrade",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"x86_32", "x86_64"}, // Prefer 32-bit
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "x86_64"}},
					},
				},
			},
			wantVersion: "2.0.0@1",
			wantArch:    "x86_64",
			wantRepo:    "repo",
		},
		{
			desc:  "complex mix",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"x86_64", "x86_32"},
			rm: RepoMap{
				"high_pri": Repo{
					Priority: 1000,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_64"}},
					},
				},
				"med_pri": Repo{
					Priority: 500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "3.0.0@1", Arch: "x86_64"}},
					},
				},
				"low_pri": Repo{ // Should win if version was primary
					Priority: 100,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "4.0.0@1", Arch: "x86_64"}},
					},
				},
			},
			wantVersion: "1.0.0@1",
			wantArch:    "x86_64",
			wantRepo:    "high_pri",
		},
		{
			desc:  "system_windows amd64 default preference",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"x86_64", "x86_32", "noarch"},
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_64"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_32"}},
					},
				},
			},
			wantVersion: "1.0.0@1",
			wantArch:    "x86_64",
			wantRepo:    "repo",
		},
		{
			desc:  "system_windows amd64 version override",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"x86_64", "x86_32", "noarch"},
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_64"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "x86_32"}},
					},
				},
			},
			wantVersion: "2.0.0@1",
			wantArch:    "x86_32",
			wantRepo:    "repo",
		},
		{
			desc:  "system_windows arm64 default preference",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"arm64", "x86_64", "x86_32", "noarch"},
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "arm64"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_64"}},
					},
				},
			},
			wantVersion: "1.0.0@1",
			wantArch:    "arm64",
			wantRepo:    "repo",
		},
		{
			desc:  "system_windows arm64 version override",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"arm64", "x86_64", "x86_32", "noarch"},
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "arm64"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "x86_64"}},
					},
				},
			},
			wantVersion: "2.0.0@1",
			wantArch:    "x86_64",
			wantRepo:    "repo",
		},
		{
			desc:  "system_windows 386 default preference",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"x86_32", "noarch"},
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "x86_32"}},
					},
				},
			},
			wantVersion: "1.0.0@1",
			wantArch:    "x86_32",
			wantRepo:    "repo",
		},
		{
			desc:  "system_windows 386 version override",
			pi:    goolib.PackageInfo{Name: "foo_pkg"},
			archs: []string{"x86_32", "noarch"},
			rm: RepoMap{
				"repo": Repo{
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0@1", Arch: "noarch"}},
						{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0@1", Arch: "x86_32"}},
					},
				},
			},
			wantVersion: "2.0.0@1",
			wantArch:    "x86_32",
			wantRepo:    "repo",
		},
	} {
		t.Run(tt.desc, func(t *testing.T) {
			gotSpec, gotRepo, gotArch, err := FindRepoLatest(tt.pi, tt.rm, tt.archs, "", false)
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
			spec, _, _, err := FindRepoLatest(tt.pi, rm, []string{"noarch"}, "", false)
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
	spec, _, _, err := FindRepoLatest(pi, rm, []string{"noarch"}, "", false)
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

func TestFindRepoLatest_LockArch(t *testing.T) {
	tests := []struct {
		name          string
		installedArch string
		isLocked      bool
		rm            RepoMap
		wantVersion   string
		wantArch      string
	}{
		{
			name:          "locked, ignore newer locked cross-arch",
			installedArch: "noarch",
			isLocked:      true,
			rm: RepoMap{
				"repo1": Repo{Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0", Arch: "x86_64", LockArch: true}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0", Arch: "noarch"}},
				}},
			},
			wantVersion: "1.0.0",
			wantArch:    "noarch",
		},
		{
			name:          "locked, accept newer unlocked cross-arch",
			installedArch: "noarch",
			isLocked:      true,
			rm: RepoMap{
				"repo1": Repo{Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0", Arch: "x86_64", LockArch: true}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "3.0.0", Arch: "x86_64", LockArch: false}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0", Arch: "noarch"}},
				}},
			},
			wantVersion: "3.0.0",
			wantArch:    "x86_64",
		},
		{
			name:          "unlocked, take newest",
			installedArch: "noarch",
			isLocked:      false,
			rm: RepoMap{
				"repo1": Repo{Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "2.0.0", Arch: "x86_64", LockArch: true}},
					{PackageSpec: &goolib.PkgSpec{Name: "foo_pkg", Version: "1.0.0", Arch: "noarch"}},
				}},
			},
			wantVersion: "2.0.0",
			wantArch:    "x86_64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, _, _, err := FindRepoLatest(goolib.PackageInfo{Name: "foo_pkg"}, tt.rm, []string{"noarch", "x86_64"}, tt.installedArch, tt.isLocked)
			if err != nil {
				t.Fatalf("FindRepoLatest failed: %v", err)
			}
			if spec.Version != tt.wantVersion {
				t.Errorf("got version %q, want %q", spec.Version, tt.wantVersion)
			}
			if spec.Arch != tt.wantArch {
				t.Errorf("got arch %q, want %q", spec.Arch, tt.wantArch)
			}
		})
	}
}

func TestNewDownloaderDoesNotModifyDefaultClient(t *testing.T) {
	defaultClient, defaultTransport := http.DefaultClient, http.DefaultClient.Transport
	t.Cleanup(func() {
		http.DefaultClient = defaultClient
		defaultClient.Transport = defaultTransport
	})
	const proxyURL = "http://proxy.example:3128"
	var clients []*http.Client
	for _, proxy := range []string{"", proxyURL} {
		d, err := NewDownloader(proxy)
		if err != nil {
			t.Fatalf("NewDownloader(%q) failed: %v", proxy, err)
		}
		if http.DefaultClient != defaultClient || http.DefaultClient.Transport != defaultTransport {
			t.Errorf("NewDownloader(%q) modified http.DefaultClient", proxy)
		}
		if d.HTTPClient == http.DefaultClient {
			t.Errorf("NewDownloader(%q).HTTPClient is http.DefaultClient", proxy)
		}
		clients = append(clients, d.HTTPClient)
	}
	if clients[0] == clients[1] {
		t.Error("two Downloaders share an http.Client")
	}
	req, err := http.NewRequest(http.MethodGet, "http://repo.example/index", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := clients[1].Transport.(*http.Transport).Proxy(req); err != nil || got.String() != proxyURL {
		t.Errorf("Proxy() = %v, %v, want %s, nil", got, err, proxyURL)
	}
}

// trackedBody records whether a response body was closed.
type trackedBody struct {
	io.ReadCloser
	closed atomic.Bool
}

func (b *trackedBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

// countingTransport counts round trips and wraps response bodies in a
// trackedBody.
type countingTransport struct {
	base   http.RoundTripper
	trips  atomic.Int32
	mu     sync.Mutex
	bodies []*trackedBody
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.trips.Add(1)
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	b := &trackedBody{ReadCloser: resp.Body}
	resp.Body = b
	c.mu.Lock()
	c.bodies = append(c.bodies, b)
	c.mu.Unlock()
	return resp, nil
}

func TestUnmarshalRepoPackagesHTTPClosesBodies(t *testing.T) {
	for _, tc := range []struct {
		desc        string
		indexExists bool
		wantErr     bool
	}{
		{"index.gz missing", true, false},
		{"index.gz and index missing", false, true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/index" && tc.indexExists {
					w.Write([]byte("[]"))
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			ct := &countingTransport{base: &http.Transport{}}
			d := &Downloader{HTTPClient: &http.Client{Transport: ct}, StallTimeout: 5 * time.Second}
			_, err := d.unmarshalRepoPackagesHTTP(context.Background(), srv.URL, filepath.Join(t.TempDir(), "cache.rs"))
			if (err != nil) != tc.wantErr {
				t.Errorf("unmarshalRepoPackagesHTTP() = %v, want error %t", err, tc.wantErr)
			}
			if len(ct.bodies) != 2 {
				t.Fatalf("unmarshalRepoPackagesHTTP() made %d requests, want 2", len(ct.bodies))
			}
			for i, b := range ct.bodies {
				if !b.closed.Load() {
					t.Errorf("response %d body not closed", i)
				}
			}
		})
	}
}

func TestGetRetry(t *testing.T) {
	stall := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	for _, tc := range []struct {
		desc      string
		handler   func(first bool, w http.ResponseWriter, r *http.Request)
		cancel    bool
		wantTrips int32
		wantErr   error
	}{
		{
			desc:      "header timeout is not retried",
			handler:   func(_ bool, w http.ResponseWriter, r *http.Request) { stall(w, r) },
			wantTrips: 1,
		},
		{
			desc: "dropped connection is retried",
			handler: func(first bool, w http.ResponseWriter, r *http.Request) {
				if !first {
					w.Write([]byte("ok"))
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("Hijack() failed: %v", err)
					return
				}
				conn.Close()
			},
			wantTrips: 2,
		},
		{
			desc:      "canceled context is not retried",
			handler:   func(_ bool, w http.ResponseWriter, r *http.Request) { stall(w, r) },
			cancel:    true,
			wantTrips: 1,
			wantErr:   context.Canceled,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reqs atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first := reqs.Add(1) == 1
				if tc.cancel {
					cancel()
				}
				tc.handler(first, w, r)
			}))
			defer srv.Close()
			ct := &countingTransport{base: &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}}
			d := &Downloader{HTTPClient: &http.Client{Transport: ct}}
			resp, err := d.Get(ctx, srv.URL)
			if err == nil {
				resp.Body.Close()
			}
			if got := ct.trips.Load(); got != tc.wantTrips {
				t.Errorf("Get() made %d requests, want %d", got, tc.wantTrips)
			}
			if wantOK := tc.wantTrips == 2; (err == nil) != wantOK {
				t.Errorf("Get() = %v, want success %t", err, wantOK)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Get() = %v, want it to wrap %v", err, tc.wantErr)
			}
		})
	}
}

func TestShouldRetry(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	reset := &url.Error{Op: "Get", URL: "http://repo", Err: errors.New("connection reset by peer")}
	dialTimeout := &url.Error{Op: "Get", URL: "http://repo", Err: &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}}
	for _, tc := range []struct {
		desc string
		ctx  context.Context
		err  error
		want bool
	}{
		{"connection reset", context.Background(), reset, true},
		{"EOF", context.Background(), &url.Error{Op: "Get", URL: "http://repo", Err: io.EOF}, true},
		{"canceled context", canceled, reset, false},
		{"expired context", expired, reset, false},
		{"dial timeout", context.Background(), dialTimeout, false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if got := shouldRetry(tc.ctx, tc.err); got != tc.want {
				t.Errorf("shouldRetry(ctx, %v) with ctx.Err() = %v = %t, want %t", tc.err, tc.ctx.Err(), got, tc.want)
			}
		})
	}
}

func TestNewRequestOauthUsesHTTPClient(t *testing.T) {
	// The token endpoint is only reachable through the Downloader's proxy.
	var tokenReqs atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "token.invalid" {
			http.NotFound(w, r)
			return
		}
		tokenReqs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "token", "token_type": "Bearer", "expires_in": 3600}`))
	}))
	defer proxy.Close()
	creds := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(creds, []byte(`{"type": "authorized_user", "client_id": "id", "client_secret": "secret", "refresh_token": "refresh", "token_uri": "http://token.invalid/token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", creds)

	d, err := NewDownloader(proxy.URL)
	if err != nil {
		t.Fatalf("NewDownloader(%q) failed: %v", proxy.URL, err)
	}
	req, err := d.NewRequest(context.Background(), http.MethodGet, "oauth-http://repo.invalid/index", nil)
	if err != nil {
		t.Fatalf("NewRequest() failed: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer token" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer token")
	}
	if got := tokenReqs.Load(); got != 1 {
		t.Errorf("got %d token requests through the proxy, want 1", got)
	}
}
