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

// Package download handles the downloading of packages.
package download

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	humanize "github.com/dustin/go-humanize"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

const (
	httpOK = 200
)

// Package downloads a package from the given url,
// the provided SHA256 checksum will be checked during download.
func Package(ctx context.Context, pkgURL, dst, chksum, proxyServer string) error {
	if err := oswrap.RemoveAll(dst); err != nil {
		return err
	}

	isGCSURL, bucket, object := goolib.SplitGCSUrl(pkgURL)
	if isGCSURL {
		return packageGCS(ctx, bucket, object, dst, chksum, "")
	}

	return packageHTTP(pkgURL, dst, chksum, proxyServer)
}

// Downloads a package from an HTTP(s) server
func packageHTTP(pkgURL, dst, chksum string, proxyServer string) error {
	httpClient := &http.Client{}
	if proxyServer != "" {
		proxyURL, err := url.Parse(proxyServer)
		if err != nil {
			logger.Fatal(err)
		}
		httpClient.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}

	resp, err := httpClient.Get(pkgURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != httpOK {
		return fmt.Errorf("Invalid return code from server, got: %d, want: %d", resp.StatusCode, httpOK)
	}

	logger.Infof("Downloading %q", pkgURL)
	return download(resp.Body, dst, chksum)
}

// Downloads a package from Google Cloud Storage
func packageGCS(ctx context.Context, bucket, object string, dst, chksum string, proxyServer string) error {
	if proxyServer != "" {
		return fmt.Errorf("Proxy server not supported with GCS URLs")
	}

	client, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	r, err := client.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		return err
	}
	defer r.Close()

	logger.Infof("Downloading gs://%s/%s", bucket, object)
	return download(r, dst, chksum)
}

// FromRepo downloads a package from a repo.
func FromRepo(ctx context.Context, rs goolib.RepoSpec, repo, dir string, proxyServer string) (string, error) {
	repoURL, err := url.Parse(repo)
	if err != nil {
		return "", err
	}
	// We strip training slashes to make sure path.Dir() removes the final component (the repo name).
	// Otherwise '/myrepo' would correctly resolve to '/', but '/myrepo/' would incorrectly resolve to '/myrepo'
	pkgURL := &url.URL{
		Scheme:  repoURL.Scheme,
		Host:    repoURL.Host,
		User:    repoURL.User,
		RawPath: path.Join(path.Dir(strings.TrimSuffix(repoURL.EscapedPath(), "/")), rs.Source),
	}
	pkgURL.Path, err = url.PathUnescape(pkgURL.RawPath)
	if err != nil {
		return "", err
	}

	pn := goolib.PackageInfo{Name: rs.PackageSpec.Name, Arch: rs.PackageSpec.Arch, Ver: rs.PackageSpec.Version}.PkgName()
	dst := filepath.Join(dir, filepath.Base(pn))
	return dst, Package(ctx, pkgURL.String(), dst, rs.Checksum, proxyServer)
}

// Latest downloads the latest available version of a package.
func Latest(ctx context.Context, name, dir string, rm client.RepoMap, archs []string, proxyServer string) (string, error) {
	ver, repo, arch, err := client.FindRepoLatest(goolib.PackageInfo{Name: name, Arch: "", Ver: ""}, rm, archs)
	if err != nil {
		return "", err
	}
	rs, err := client.FindRepoSpec(goolib.PackageInfo{Name: name, Arch: arch, Ver: ver}, rm[repo])
	if err != nil {
		return "", err
	}
	return FromRepo(ctx, rs, repo, dir, proxyServer)
}

func download(r io.Reader, dst, chksum string) (err error) {
	f, err := oswrap.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cErr := f.Close(); cErr != nil && err == nil {
			err = cErr
		}
	}()

	hash := sha256.New()
	tw := io.MultiWriter(f, hash)

	b, err := io.Copy(tw, r)
	if err != nil {
		return err
	}

	if hex.EncodeToString(hash.Sum(nil)) != chksum {
		fmt.Println(hex.EncodeToString(hash.Sum(nil)), chksum)
		return errors.New("checksum of downloaded file does not match expected checksum")
	}

	logger.Infof("Successfully downloaded %s", humanize.IBytes(uint64(b)))
	return nil
}

// ExtractPkg takes a path to a package and extracts it to a directory based on the
// package name, it returns the path to the extraced directory.
func ExtractPkg(src string) (dst string, err error) {
	dst = strings.TrimSuffix(src, filepath.Ext(src))
	if src == "" || dst == "" {
		return "", fmt.Errorf("package extraction paths are invalid: src %s, dst %s", src, dst)
	}
	if err := oswrap.Mkdir(dst, 0755); err != nil && !os.IsExist(err) {
		return "", err
	}
	logger.Infof("Extracting %q to %q", src, dst)

	f, err := oswrap.Open(src)
	if err != nil {
		return "", fmt.Errorf("error reading zip package: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		if !os.IsExist(err) {
			return "", err
		}
	}
	tr := tar.NewReader(gr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("error opening file: %v", err)
		}

		name := filepath.Clean(header.Name)
		if name[0:3] == ".."+string(os.PathSeparator) {
			return "", fmt.Errorf("error unpacking package, file contains path traversal: %q", name)
		}

		path := filepath.Join(dst, name)
		if header.FileInfo().IsDir() {
			if err := oswrap.MkdirAll(path, 0755); err != nil {
				return "", err
			}
			continue
		}
		if err := oswrap.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", err
		}
		f, err := oswrap.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode))
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
	}
	return dst, nil
}
