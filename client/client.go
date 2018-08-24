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

// Package client contains common functions for the GooGet client.
package client

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
	"google.golang.org/api/googleapi"
)

// PackageState describes the state of a package on a client.
type PackageState struct {
	SourceRepo, DownloadURL, Checksum, LocalPath, UnpackDir string
	PackageSpec                                             *goolib.PkgSpec
	InstalledFiles                                          map[string]string
}

// GooGetState describes the overall package state on a client.
type GooGetState []PackageState

// Add appends a PackageState.
func (s *GooGetState) Add(ps PackageState) {
	*s = append(*s, ps)
}

// Remove removes a PackageState.
func (s *GooGetState) Remove(pi goolib.PackageInfo) error {
	for i, ps := range *s {
		if ps.Match(pi) {
			(*s)[i] = (*s)[len(*s)-1]
			*s = (*s)[:len(*s)-1]
			return nil
		}
	}
	return fmt.Errorf("no match found for package %s.%s.%s in state", pi.Name, pi.Arch, pi.Ver)
}

// GetPackageState returns the PackageState of the matching goolib.PackageInfo,
// or error if no match is found.
func (s *GooGetState) GetPackageState(pi goolib.PackageInfo) (PackageState, error) {
	for _, ps := range *s {
		if ps.Match(pi) {
			return ps, nil
		}
	}
	return PackageState{}, fmt.Errorf("no match found for package %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
}

// Marshal JSON marshals GooGetState.
func (s *GooGetState) Marshal() ([]byte, error) {
	return json.Marshal(s)
}

// UnmarshalState unmarshals data into GooGetState.
func UnmarshalState(b []byte) (*GooGetState, error) {
	var s GooGetState
	return &s, json.Unmarshal(b, &s)
}

// Match reports whether the PackageState corresponds to the package info.
func (ps *PackageState) Match(pi goolib.PackageInfo) bool {
	return ps.PackageSpec.Name == pi.Name && (ps.PackageSpec.Arch == pi.Arch || pi.Arch == "") && (ps.PackageSpec.Version == pi.Ver || pi.Ver == "")
}

// RepoMap describes each repo's packages as seen from a client.
type RepoMap map[string][]goolib.RepoSpec

// AvailableVersions builds a RepoMap from a list of sources.
func AvailableVersions(ctx context.Context, srcs []string, cacheDir string, cacheLife time.Duration, proxyServer string) RepoMap {
	rm := make(RepoMap)
	for _, r := range srcs {
		rf, err := unmarshalRepoPackages(ctx, r, cacheDir, cacheLife, proxyServer)
		if err != nil {
			logger.Errorf("error reading repo %q: %v", r, err)
			continue
		}
		rm[r] = rf
	}
	return rm
}

func decode(index io.ReadCloser, ct, url, cf string) ([]goolib.RepoSpec, error) {
	defer index.Close()

	var dec *json.Decoder
	switch ct {
	case "application/x-gzip":
		gr, err := gzip.NewReader(index)
		if err != nil {
			return nil, err
		}
		dec = json.NewDecoder(gr)
	case "application/json":
		dec = json.NewDecoder(index)
	default:
		return nil, fmt.Errorf("unsupported content type: %s", ct)
	}

	var m []goolib.RepoSpec
	for dec.More() {
		if err := dec.Decode(&m); err != nil {
			return nil, err
		}
	}

	f, err := oswrap.Create(cf)
	if err != nil {
		return nil, err
	}
	j, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(j); err != nil {
		return nil, err
	}

	// The .url files aren't used by googet but help developers and the
	// curious figure out which file belongs to which repo/URL.
	mf := fmt.Sprintf("%s.url", strings.TrimSuffix(cf, filepath.Ext(cf)))
	if err = ioutil.WriteFile(mf, []byte(url), 0644); err != nil {
		logger.Errorf("Failed to write '%s': %v", mf, err)
	}

	return m, f.Close()
}

// unmarshalRepoPackages gets and unmarshals a repository URL or uses the cached contents
// if mtime is less than cacheLife.
// Sucessfully unmarshalled contents will be written to a cache.
func unmarshalRepoPackages(ctx context.Context, p, cacheDir string, cacheLife time.Duration, proxyServer string) ([]goolib.RepoSpec, error) {

	cf := filepath.Join(cacheDir, fmt.Sprintf("%x.rs", sha256.Sum256([]byte(p))))

	fi, err := oswrap.Stat(cf)
	if err == nil && time.Since(fi.ModTime()) < cacheLife {
		logger.Infof("Using cached repo content for %s.", p)
		f, err := oswrap.Open(cf)
		if err != nil {
			return nil, err
		}
		var m []goolib.RepoSpec
		dec := json.NewDecoder(f)
		for dec.More() {
			if err := dec.Decode(&m); err != nil {
				return nil, err
			}
		}
		return m, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	logger.Infof("Fetching repo content for %s, cache either doesn't exist or is older than %v", p, cacheLife)

	isGCSURL, bucket, object := goolib.SplitGCSUrl(p)
	if isGCSURL {
		return unmarshalRepoPackagesGCS(ctx, bucket, object, p, cf, proxyServer)
	}
	return unmarshalRepoPackagesHTTP(p, cf, proxyServer)
}

func unmarshalRepoPackagesHTTP(repoURL string, cf string, proxyServer string) ([]goolib.RepoSpec, error) {
	httpClient := &http.Client{}
	if proxyServer != "" {
		proxyURL, err := url.Parse(proxyServer)
		if err != nil {
			logger.Fatal(err)
		}
		httpClient.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}

	indexURL := repoURL + "/index.gz"
	ct := "application/x-gzip"
	logger.Infof("Fetching %q", indexURL)
	res, err := httpClient.Get(indexURL)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != 200 {
		logger.Infof("Gzipped index returned status: %q, trying plain JSON.", res.Status)
		indexURL = repoURL + "/index"
		ct = "application/json"
		logger.Infof("Fetching %q", indexURL)
		res, err = httpClient.Get(indexURL)
		if err != nil {
			return nil, err
		}

		if res.StatusCode != 200 {
			return nil, fmt.Errorf("index GET request returned status: %q", res.Status)
		}
	}

	return decode(res.Body, ct, repoURL, cf)
}

func unmarshalRepoPackagesGCS(ctx context.Context, bucket, object, url, cf string, proxyServer string) ([]goolib.RepoSpec, error) {
	if proxyServer != "" {
		logger.Errorf("Proxy server not supported with gs:// URLs, skiping repo 'gs://%s/%s'", bucket, object)
		var empty []goolib.RepoSpec
		return empty, nil
	}

	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	bkt := client.Bucket(bucket)
	if len(object) != 0 {
		object += "/"
	}

	indexPath := object + "index.gz"
	logger.Infof("Fetching 'gs://%s/%s", bucket, indexPath)
	if r, err := bkt.Object(indexPath).NewReader(ctx); err == nil {
		return decode(r, "application/x-gzip", url, cf)
	}

	if gErr, ok := err.(*googleapi.Error); ok && gErr.Code != http.StatusNotFound {
		return nil, err
	}

	logger.Info("Failed to read gzipped index, trying plain JSON.")
	indexPath = object + "index"
	r, err := bkt.Object(indexPath).NewReader(ctx)
	if err != nil {
		return nil, err
	}

	return decode(r, "application/json", url, cf)
}

// FindRepoSpec returns the element of pl whose PackageSpec matches pi.
func FindRepoSpec(pi goolib.PackageInfo, pl []goolib.RepoSpec) (goolib.RepoSpec, error) {
	for _, p := range pl {
		ps := p.PackageSpec
		if ps.Name == pi.Name && ps.Arch == pi.Arch && ps.Version == pi.Ver {
			return p, nil
		}
	}
	return goolib.RepoSpec{}, fmt.Errorf("no match found for package %s.%s.%s in repo", pi.Name, pi.Arch, pi.Ver)
}

func latest(psm map[string][]*goolib.PkgSpec) (ver, repo string) {
	for r, pl := range psm {
		for _, p := range pl {
			if ver == "" {
				repo = r
				ver = p.Version
				continue
			}
			c, err := goolib.Compare(p.Version, ver)
			if err != nil {
				logger.Errorf("compare of %s to %s failed with error: %v", p.Version, ver, err)
			}
			if c == 1 {
				repo = r
				ver = p.Version
			}
		}
	}
	return
}

// FindRepoLatest returns the latest version of a package along with its repo and arch.
func FindRepoLatest(pi goolib.PackageInfo, rm RepoMap, archs []string) (ver, repo, arch string, err error) {
	psm := make(map[string][]*goolib.PkgSpec)
	if pi.Arch != "" {
		for r, pl := range rm {
			for _, p := range pl {
				if p.PackageSpec.Name == pi.Name && p.PackageSpec.Arch == pi.Arch {
					psm[r] = append(psm[r], p.PackageSpec)
				}
			}
		}
		if len(psm) != 0 {
			v, r := latest(psm)
			return v, r, pi.Arch, nil
		}
		return "", "", "", fmt.Errorf("no versions of package %s.%s found in any repo", pi.Name, pi.Arch)
	}

	for _, a := range archs {
		for r, pl := range rm {
			for _, p := range pl {
				if p.PackageSpec.Name == pi.Name && p.PackageSpec.Arch == a {
					psm[r] = append(psm[r], p.PackageSpec)
				}
			}
		}
		if len(psm) != 0 {
			v, r := latest(psm)
			return v, r, a, nil
		}
	}
	return "", "", "", fmt.Errorf("no versions of package %s found in any repo", pi.Name)
}

// WhatRepo returns what repo a package is in.
// Name, Arch, and Ver fields of PackageInfo must be provided.
func WhatRepo(pi goolib.PackageInfo, rm RepoMap) (string, error) {
	for r, pl := range rm {
		for _, p := range pl {
			if p.PackageSpec.Name == pi.Name && p.PackageSpec.Arch == pi.Arch && p.PackageSpec.Version == pi.Ver {
				return r, nil
			}
		}
	}
	return "", fmt.Errorf("package %s %s version %s not found in any repo", pi.Arch, pi.Name, pi.Ver)
}

// RemoveOrRename attempts to remove a file or directory. If it fails
// and it's a file, attempt to rename it into a temp file on windows so
// that it can be effectively overridden returning the name of the temp file.
func RemoveOrRename(filename string) (string, error) {
	rmErr := oswrap.Remove(filename)
	if rmErr == nil || os.IsNotExist(rmErr) {
		return "", nil
	}
	fi, err := oswrap.Stat(filename)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", rmErr
	}

	tmpDir := os.TempDir()
	if filepath.VolumeName(tmpDir) != filepath.VolumeName(filename) {
		tmpDir = filepath.Dir(filename)
	}

	tmpFile, err := ioutil.TempFile(tmpDir, filepath.Base(filename)+".old")
	if err != nil {
		return "", err
	}
	newName := tmpFile.Name()
	tmpFile.Close()
	if err := oswrap.Remove(newName); err != nil {
		return "", err
	}
	if err := oswrap.Rename(filename, newName); err != nil {
		return "", err
	}
	return newName, oswrap.RemoveOnReboot(newName)
}
