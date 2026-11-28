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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/priority"
	"github.com/google/logger"
	"golang.org/x/oauth2"
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
	// HTTPClient makes the Downloader's HTTP requests, including the oauth
	// token requests for "oauth-" URLs. Requests to the GCE metadata server
	// use the cloud.google.com/go/compute/metadata package's own client.
	HTTPClient       *http.Client
	UsingProxyServer bool
	// StallTimeout is how long a response body may deliver no data, or a GCS
	// object take to open, before the transfer is canceled. NewDownloader
	// sets it, along with the response header timeout, to the package-level
	// StallTimeout; zero or less falls back to StallTimeout in OpenGCS and
	// NewStallReader.
	StallTimeout time.Duration
	// ReadOnlyCache makes the Downloader use the repo index cache without
	// writing it, for runs that may not change the googet root. A fresh cache
	// entry is used as usual. A stale, missing, empty or undecodable entry,
	// or one that can't be read because a writer is replacing it, is fetched
	// into memory and the cache is left as it was: no cache or .url file is
	// written, and no folder is created. The zero value, false, writes the
	// cache, as library users expect.
	ReadOnlyCache bool
}

// NewDownloader returns a Downloader optionally using a specified proxyServer.
// The Downloader gets its own http.Client; http.DefaultClient is not modified.
func NewDownloader(proxyServer string) (*Downloader, error) {
	proxy := http.ProxyFromEnvironment
	if proxyServer != "" {
		proxyURL, err := url.Parse(proxyServer)
		if err != nil {
			return nil, err
		}
		proxy = http.ProxyURL(proxyURL)
	}
	timeout := stallTimeout(0)
	transport := &http.Transport{
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
		// Response bodies are guarded by NewStallReader; this bounds the
		// wait for the response headers.
		ResponseHeaderTimeout: timeout,
	}
	return &Downloader{
		HTTPClient:       &http.Client{Transport: transport},
		UsingProxyServer: proxyServer != "",
		StallTimeout:     timeout,
	}, nil
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

// repoCacheFile returns the path in cacheDir of the cached index for repoURL.
// An "oauth-" prefix doesn't change the name.
func repoCacheFile(cacheDir, repoURL string) string {
	pName := strings.TrimPrefix(repoURL, "oauth-")
	return filepath.Join(cacheDir, fmt.Sprintf("%x.rs", sha256.Sum256([]byte(pName))))
}

// unmarshalRepoPackages gets and unmarshals a repository URL or uses the cached contents
// if mtime is less than cacheLife.
// Successfully unmarshalled contents are written to a cache, unless
// d.ReadOnlyCache is set; a failure to write the cache is logged and doesn't
// fail the call.
// A cache entry that is empty or can't be decoded, or that can't be read
// because a writer is replacing or removing it, counts as a cache miss.
func (d *Downloader) unmarshalRepoPackages(ctx context.Context, p, cacheDir string, cacheLife time.Duration) ([]goolib.RepoSpec, error) {
	pName := strings.TrimPrefix(p, "oauth-")

	cf := repoCacheFile(cacheDir, p)

	fi, err := oswrap.Stat(cf)
	stale := err == nil && time.Since(fi.ModTime()) >= cacheLife
	if err == nil && !stale {
		rs, err := readCache(cf)
		if err == nil {
			logger.Infof("Using cached repo content for %s.", pName)
			return rs, nil
		}
		if !isCacheMiss(err) {
			return nil, err
		}
		logger.Infof("Ignoring cached repo content for %s: %v", pName, err)
	} else if err != nil && !isCacheMiss(err) {
		return nil, err
	}
	logger.Infof("Fetching repo content for %s, cache either doesn't exist or is older than %v", pName, cacheLife)
	if d.ReadOnlyCache && stale {
		logger.Infof("Not refreshing the stale cached repo content for %s, because the cache is read-only, as it is in every unelevated run; an elevated run refreshes it.", pName)
	}

	isGCSURL, bucket, object := goolib.SplitGCSUrl(pName)
	var rs []goolib.RepoSpec
	if isGCSURL {
		if d.UsingProxyServer {
			logger.Errorf("Proxy server not supported with gs:// URLs, skipping repo 'gs://%s/%s'", bucket, object)
			var empty []goolib.RepoSpec
			return empty, nil
		}
		rs, err = d.unmarshalRepoPackagesGCS(ctx, bucket, object)
	} else {
		rs, err = d.unmarshalRepoPackagesHTTP(ctx, p)
	}
	if err != nil {
		return nil, err
	}
	if d.ReadOnlyCache {
		return rs, nil
	}
	// The cache only saves a later fetch, so a failure to write it doesn't
	// fail the fetch that already succeeded.
	if err := writeCache(rs, p, cf); err != nil {
		logger.Errorf("Failed to cache repo content for %s: %v", pName, err)
	}
	return rs, nil
}

// errBadCache reports a cache file that is empty or can't be decoded, for
// example because a writer crashed or a reader raced an older, non-atomic
// writer.
var errBadCache = errors.New("cache file is empty or corrupt")

// isCacheMiss reports whether err, from reading the cache file, means the
// cache should be ignored and the repo fetched again: the file is missing,
// empty or corrupt, or it is being replaced or removed by an elevated writer
// (a transient Windows sharing error, see oswrap.IsTransientShareError).
func isCacheMiss(err error) bool {
	return errors.Is(err, errBadCache) || os.IsNotExist(err) || oswrap.IsTransientShareError(err)
}

// readCache reads and decodes the cache file cf. It reads the whole file and
// closes it before decoding, so the handle never blocks a writer replacing the
// cache. An empty or undecodable file gives an error wrapping errBadCache.
func readCache(cf string) ([]goolib.RepoSpec, error) {
	b, err := oswrap.ReadFileShared(cf)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, errBadCache
	}
	rs, err := decode(io.NopCloser(bytes.NewReader(b)), "application/json")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadCache, err)
	}
	return rs, nil
}

// writeCache writes rs to the cache file cf, replacing it atomically, so that
// a concurrent reader never sees a partial file. It also writes repo to a .url
// file next to cf; a failure there is only logged.
func writeCache(rs []goolib.RepoSpec, repo, cf string) error {
	j, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	if err := oswrap.WriteFileAtomic(cf, j, 0644); err != nil {
		return err
	}

	// The .url files aren't used by googet but help identify which file belongs to which repo/URL.
	mf := fmt.Sprintf("%s.url", strings.TrimSuffix(cf, filepath.Ext(cf)))
	if err := oswrap.WriteFileAtomic(mf, []byte(repo), 0644); err != nil {
		logger.Errorf("Failed to write '%s': %v", mf, err)
	}
	return nil
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
		// Fetch credentials and tokens through d.HTTPClient, so that they use
		// the configured proxy and timeouts rather than http.DefaultClient.
		ctx := context.WithValue(ctx, oauth2.HTTPClient, d.HTTPClient)
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
		return false, 0, fmt.Errorf("HEAD request returned status: %q", resp.Status)
	}
	if resp.Header.Get("Accept-Ranges") == "bytes" && resp.ContentLength >= 0 {
		return true, resp.ContentLength, nil
	}
	return false, 0, nil
}

// Get gets a url using an optional proxy server. It retries once on a
// transport error, unless ctx is done or the error is a timeout; see
// shouldRetry.
func (d *Downloader) Get(ctx context.Context, path string) (*http.Response, error) {
	req, err := d.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.HTTPClient.Do(req)
	if err == nil || !shouldRetry(ctx, err) {
		return resp, err
	}
	return d.HTTPClient.Do(req)
}

// shouldRetry reports whether Get should retry a request that failed with
// err. A single retry mitigates some connection issues, such as a reset or
// an EOF on a reused connection. Timeouts are not retried, so that a server
// that stalls before sending the response headers isn't waited on twice;
// this also covers dial and TLS handshake timeouts. Requests whose context
// is done are not retried either.
func shouldRetry(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var netErr net.Error
	return !errors.As(err, &netErr) || !netErr.Timeout()
}

func (d *Downloader) unmarshalRepoPackagesHTTP(ctx context.Context, repoURL string) ([]goolib.RepoSpec, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	indexURL := repoURL + "/index.gz"
	trimmedIndexURL := strings.TrimPrefix(indexURL, "oauth-")
	ct := "application/x-gzip"
	logger.Infof("Fetching %q", trimmedIndexURL)
	res, err := d.Get(ctx, indexURL)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		indexURL = repoURL + "/index"
		trimmedIndexURL = strings.TrimPrefix(indexURL, "oauth-")
		ct = "application/json"
		logger.Infof("Fetching %q", trimmedIndexURL)
		res, err = d.Get(ctx, indexURL)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return nil, fmt.Errorf("index GET request returned status: %q", res.Status)
		}
	}
	return decode(NewStallReader(res.Body, d.StallTimeout, func() { cancel(ErrStalled) }), ct)
}

func (d *Downloader) unmarshalRepoPackagesGCS(ctx context.Context, bucket, object string) ([]goolib.RepoSpec, error) {
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
	r, _, err := d.OpenGCS(ctx, bkt.Object(indexPath))
	if err == nil {
		return decode(r, "application/x-gzip")
	}

	var gErr *googleapi.Error
	if !errors.Is(err, storage.ErrObjectNotExist) && (!errors.As(err, &gErr) || gErr.Code != http.StatusNotFound) {
		return nil, err
	}

	logger.Info("Failed to read gzipped index, trying plain JSON.")
	indexPath = object + "index"
	r, _, err = d.OpenGCS(ctx, bkt.Object(indexPath))
	if err != nil {
		return nil, err
	}

	return decode(r, "application/json")
}

// decode decodes the repo index read from index, whose content type is ct, and
// closes index. It writes nothing.
func decode(index io.ReadCloser, ct string) ([]goolib.RepoSpec, error) {
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
	// Decode until io.EOF rather than looping on dec.More, which reports false
	// on a read error and so would let a stalled or truncated index be cached.
	for {
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return m, nil
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
// The search order is:
// 1. Repo Priority (High > Low)
// 2. Version (New > Old)
// 3. Architecture Preference (as defined by archs slice order)
func FindRepoLatest(pi goolib.PackageInfo, rm RepoMap, archs []string, installedArch string, isLocked bool) (*goolib.PkgSpec, string, string, error) {
	name := pi.Name
	if pi.Arch != "" {
		archs = []string{pi.Arch}
		name = fmt.Sprintf("%s.%s", pi.Name, pi.Arch)
	}

	archPref := make(map[string]int)
	for i, a := range archs {
		archPref[a] = i
	}

	type candidate struct {
		spec     *goolib.PkgSpec
		repo     string
		priority priority.Value
	}
	var directCandidates []candidate
	var providesCandidates []candidate

	for u, r := range rm {
		for _, p := range r.Packages {
			ps := p.PackageSpec

			if _, ok := archPref[ps.Arch]; !ok {
				continue
			}

			if ps.Name == pi.Name {
				if satisfiesVersion(ps.Version, pi.Ver) {
					directCandidates = append(directCandidates, candidate{ps, u, r.Priority})
				}
				continue
			}

			for _, prov := range ps.Provides {
				if SatisfiesProvider(prov, pi.Name, pi.Ver) {
					providesCandidates = append(providesCandidates, candidate{ps, u, r.Priority})
					break
				}
			}
		}
	}

	cmpFunc := func(a, b candidate) int {
		c, err := goolib.ComparePriorityVersion(a.priority, a.spec.Version, b.priority, b.spec.Version)
		if err != nil {
			logger.Errorf("Error comparing priority/version: %v", err)
			return 0
		}
		if c != 0 {
			return -c // reverse for descending order
		}
		if archPref[a.spec.Arch] < archPref[b.spec.Arch] {
			return -1
		}
		if archPref[a.spec.Arch] > archPref[b.spec.Arch] {
			return 1
		}
		return 0
	}

	bestCandidate := func(list []candidate) (*goolib.PkgSpec, string, string, error) {
		if len(list) == 0 {
			return nil, "", "", fmt.Errorf("no package found")
		}
		slices.SortFunc(list, cmpFunc)
		for _, cand := range list {
			if isLocked && cand.spec.Arch != installedArch && cand.spec.LockArch {
				continue // Ignore this candidate
			}
			return cand.spec, cand.repo, cand.spec.Arch, nil
		}
		return nil, "", "", fmt.Errorf("no package found satisfying lock conditions")
	}

	for _, cands := range [][]candidate{directCandidates, providesCandidates} {
		if len(cands) == 0 {
			continue
		}
		spec, repo, arch, err := bestCandidate(cands)
		if err == nil {
			return spec, repo, arch, nil
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

	return pName == reqName && satisfiesVersion(pVer, reqVer)
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

// RemoveOrRename calls oswrap.RemoveOrPark with os.TempDir() as the park
// directory; see oswrap.RemoveOrPark.
func RemoveOrRename(filename string) (string, error) {
	return oswrap.RemoveOrPark(filename, os.TempDir())
}
