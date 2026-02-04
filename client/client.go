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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/priority"
	"github.com/google/logger"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
)

// InstalledApplication describes the mapped Windows application to the package
type InstalledApplication struct {
	// Display Name of the installed application found in the registry
	Name string
	// Registry key of the installed application in uninstall
	Reg string
}

// PackageState describes the state of a package on a client.
type PackageState struct {
	SourceRepo, DownloadURL, Checksum, LocalPath, UnpackDir string
	PackageSpec                                             *goolib.PkgSpec
	InstalledFiles                                          map[string]string
	InstallDate                                             int64
	InstalledApp                                            InstalledApplication
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
		if ps.PackageSpec == nil {
			continue
		}
		if ps.Match(pi) {
			return ps, nil
		}
	}
	return PackageState{}, fmt.Errorf("no match found for package %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
}

// PackageMap is a map of package "name.arch" to package version.
type PackageMap map[string]string

// PackageMap returns a PackageMap of all installed packages based on the
// GooGetState.
func (s GooGetState) PackageMap() PackageMap {
	pm := make(PackageMap)
	for _, p := range s {
		pm[p.PackageSpec.Name+"."+p.PackageSpec.Arch] = p.PackageSpec.Version
	}
	return pm
}

// Match reports whether the PackageState corresponds to the package info.
func (ps *PackageState) Match(pi goolib.PackageInfo) bool {
	return ps.PackageSpec.Name == pi.Name && (ps.PackageSpec.Arch == pi.Arch || pi.Arch == "") && (ps.PackageSpec.Version == pi.Ver || pi.Ver == "")
}

// Repo represents a single downloaded repo.
type Repo struct {
	Priority priority.Value
	Packages []goolib.RepoSpec
}

// RepoMap describes each repo's packages as seen from a client.
type RepoMap map[string]Repo

// Downloader is a wrapper around http.Client
type Downloader struct {
	HTTPClient       *http.Client
	UsingProxyServer bool
}

// NewDownloader returns a Downloader optionally using a specified proxyServer.
func NewDownloader(proxyServer string) (*Downloader, error) {
	httpClient := http.DefaultClient
	proxy := http.ProxyFromEnvironment
	if proxyServer != "" {
		proxyURL, err := url.Parse(proxyServer)
		if err != nil {
			return nil, err
		}
		proxy = http.ProxyURL(proxyURL)
	}
	httpClient.Transport = &http.Transport{
		Proxy: proxy,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Downloader{HTTPClient: httpClient, UsingProxyServer: proxyServer != ""}, nil
}

// AvailableVersions builds a RepoMap from a list of sources.
func (d *Downloader) AvailableVersions(ctx context.Context, srcs map[string]priority.Value, cacheDir string, cacheLife time.Duration) RepoMap {
	rm := make(RepoMap)
	for r, pri := range srcs {
		rf, err := d.unmarshalRepoPackages(ctx, r, cacheDir, cacheLife)
		if err != nil {
			logger.Errorf("error reading repo %q: %v", r, err)
			continue
		}
		rm[r] = Repo{
			Priority: pri,
			Packages: rf,
		}
	}
	return rm
}

// unmarshalRepoPackages gets and unmarshals a repository URL or uses the cached contents
// if mtime is less than cacheLife.
// Successfully unmarshalled contents will be written to a cache.
func (d *Downloader) unmarshalRepoPackages(ctx context.Context, p, cacheDir string, cacheLife time.Duration) ([]goolib.RepoSpec, error) {
	pName := strings.TrimPrefix(p, "oauth-")

	cf := filepath.Join(cacheDir, fmt.Sprintf("%x.rs", sha256.Sum256([]byte(pName))))

	fi, err := oswrap.Stat(cf)
	if err == nil && time.Since(fi.ModTime()) < cacheLife {
		logger.Infof("Using cached repo content for %s.", pName)
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
	logger.Infof("Fetching repo content for %s, cache either doesn't exist or is older than %v", pName, cacheLife)

	isGCSURL, bucket, object := goolib.SplitGCSUrl(pName)
	if isGCSURL {
		return d.unmarshalRepoPackagesGCS(ctx, bucket, object, pName, cf)
	}
	return d.unmarshalRepoPackagesHTTP(ctx, p, cf)
}

// NewRequest returns an http.Request, adding auth headers if the URL is
// prefixed with "oauth-".
func (d *Downloader) NewRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	u, useOauth := strings.CutPrefix(url, "oauth-")
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if useOauth {
		creds, err := google.FindDefaultCredentials(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to obtain creds: %v", err)
		}
		token, err := creds.TokenSource.Token()
		if err != nil {
			return nil, fmt.Errorf("failed to obtain access token: %v", err)
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))
	}
	return req, nil
}

// CanResume returns true if we can resume downloading the specified url.
func (d *Downloader) CanResume(ctx context.Context, url string) (bool, int64, error) {
	req, err := d.NewRequest(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, 0, err
	}
	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return false, 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, 0, err
	}
	if resp.Header.Get("Accept-Ranges") == "bytes" && resp.ContentLength >= 0 {
		return true, resp.ContentLength, nil
	}
	return false, 0, nil
}

// Get gets a url using an optional proxy server, retrying once on any error.
func (d *Downloader) Get(ctx context.Context, path string) (*http.Response, error) {
	req, err := d.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.HTTPClient.Do(req)
	// We retry on any error once as this mitigates some
	// connection issues in certain situations.
	if err == nil {
		return resp, nil
	}
	return d.HTTPClient.Do(req)
}

func (d *Downloader) unmarshalRepoPackagesHTTP(ctx context.Context, repoURL string, cf string) ([]goolib.RepoSpec, error) {
	indexURL := repoURL + "/index.gz"
	trimmedIndexURL := strings.TrimPrefix(indexURL, "oauth-")
	ct := "application/x-gzip"
	logger.Infof("Fetching %q", trimmedIndexURL)
	res, err := d.Get(ctx, indexURL)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		indexURL = repoURL + "/index"
		trimmedIndexURL = strings.TrimPrefix(indexURL, "oauth-")
		ct = "application/json"
		logger.Infof("Fetching %q", trimmedIndexURL)
		res, err = d.Get(ctx, indexURL)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("index GET request returned status: %q", res.Status)
		}
	}
	return decode(res.Body, ct, repoURL, cf)
}

func (d *Downloader) unmarshalRepoPackagesGCS(ctx context.Context, bucket, object, url, cf string) ([]goolib.RepoSpec, error) {
	if d.UsingProxyServer {
		logger.Errorf("Proxy server not supported with gs:// URLs, skipping repo 'gs://%s/%s'", bucket, object)
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

// FindRepoSpec returns the RepoSpec in repo whose PackageSpec matches pi.
func FindRepoSpec(pi goolib.PackageInfo, repo Repo) (goolib.RepoSpec, error) {
	for _, p := range repo.Packages {
		ps := p.PackageSpec
		if ps.Name == pi.Name && ps.Arch == pi.Arch && ps.Version == pi.Ver {
			return p, nil
		}
	}
	return goolib.RepoSpec{}, fmt.Errorf("no match found for package %s.%s.%s in repo", pi.Name, pi.Arch, pi.Ver)
}

// latest returns the package spec and repo having the greatest (priority, version) from the set of
// package specs in psm.
func latest(psm map[string][]*goolib.PkgSpec, rm RepoMap) (*goolib.PkgSpec, string) {
	var bestPkg *goolib.PkgSpec
	var repoURL string
	var pri priority.Value
	for u, pl := range psm {
		for _, pkg := range pl {
			q := rm[u].Priority
			c := 1
			if bestPkg != nil {
				var err error
				if c, err = goolib.ComparePriorityVersion(q, pkg.Version, pri, bestPkg.Version); err != nil {
					logger.Errorf("compare of %s to %s failed with error: %v", pkg.Version, bestPkg.Version, err)
					continue
				}
			}
			if c == 1 {
				repoURL = u
				bestPkg = pkg
				pri = q
			}
		}
	}
	return bestPkg, repoURL
}

// FindRepoLatest returns the latest version of a package along with its repo and arch.
// It checks both direct name matches and "Provides" entries.
// The archs are searched in order; if a matching package is found for any arch, it is
// returned immediately even if a later arch might have a later version.
func FindRepoLatest(pi goolib.PackageInfo, rm RepoMap, archs []string) (*goolib.PkgSpec, string, string, error) {
	name := pi.Name
	if pi.Arch != "" {
		archs = []string{pi.Arch}
		name = fmt.Sprintf("%s.%s", pi.Name, pi.Arch)
	}

	for _, a := range archs {
		psmDirect := make(map[string][]*goolib.PkgSpec)
		psmProvides := make(map[string][]*goolib.PkgSpec)

		for u, r := range rm {
			for _, p := range r.Packages {
				ps := p.PackageSpec
				if ps.Arch != a {
					continue
				}

				// Check exact match
				if ps.Name == pi.Name {
					if satisfiesVersion(ps.Version, pi.Ver) {
						psmDirect[u] = append(psmDirect[u], ps)
					}
					// Skip checking Provides if the package itself is a direct match.
					continue
				}

				// Check provides
				for _, prov := range ps.Provides {
					if SatisfiesProvider(prov, pi.Name, pi.Ver) {
						psmProvides[u] = append(psmProvides[u], ps)
						break
					}
				}
			}
		}

		// Prioritize direct package matches over virtual package providers.
		if len(psmDirect) > 0 {
			pkg, repo := latest(psmDirect, rm)
			if pkg != nil {
				return pkg, repo, a, nil
			}
		}

		// If no direct matches, check providers.
		// Note: This matches Arch behavior (prefer real package).
		if len(psmProvides) > 0 {
			pkg, repo := latest(psmProvides, rm)
			if pkg != nil {
				return pkg, repo, a, nil
			}
		}
	}
	return nil, "", "", fmt.Errorf("no package found satisfying %s in any repo", name)
}

func satisfiesVersion(pkgVer, reqVer string) bool {
	if reqVer == "" || pkgVer == "" {
		return true
	}
	c, err := goolib.Compare(pkgVer, reqVer)
	if err != nil {
		logger.Errorf("Error comparing versions %s vs %s: %v", pkgVer, reqVer, err)
		return false
	}
	return c >= 0
}

// SatisfiesProvider checks if a provider string satisfies a requirement.
func SatisfiesProvider(prov, reqName, reqVer string) bool {
	pName, pVer, _ := strings.Cut(prov, "=")

	if pName != reqName {
		return false
	}

	return satisfiesVersion(pVer, reqVer)
}

// WhatRepo returns what repo a package is in.
// Name, Arch, and Ver fields of PackageInfo must be provided.
func WhatRepo(pi goolib.PackageInfo, rm RepoMap) (string, error) {
	for u, r := range rm {
		for _, p := range r.Packages {
			if p.PackageSpec.Name == pi.Name && p.PackageSpec.Arch == pi.Arch && p.PackageSpec.Version == pi.Ver {
				return u, nil
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
