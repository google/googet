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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	humanize "github.com/dustin/go-humanize"
	"github.com/google/googet/client"
	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
)

// Package downloads a package from the given url,
// if a SHA256 checksum is provided it will be checked.
func Package(pkgURL, dst, chksum string, proxyServer string) error {
	httpClient := &http.Client{}
	if proxyServer != "" {
		proxyURL, err := url.Parse(proxyServer)
		if err != nil {
			logger.Fatalf("%q", err)
		}
		httpClient.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}
	resp, err := httpClient.Get(pkgURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	logger.Infof("Downloading %q", pkgURL)
	if err := oswrap.RemoveAll(dst); err != nil {
		return err
	}
	if err := download(resp.Body, dst, chksum, proxyServer); err != nil {
		return err
	}
	return nil
}

// FromRepo downloads a package from a repo.
func FromRepo(rs goolib.RepoSpec, repo, dir string, proxyServer string) (string, error) {
	pkgURL := strings.TrimSuffix(repo, filepath.Base(repo)) + rs.Source
	pn := goolib.PackageInfo{rs.PackageSpec.Name, rs.PackageSpec.Arch, rs.PackageSpec.Version}.PkgName()
	dst := filepath.Join(dir, filepath.Base(pn))
	return dst, Package(pkgURL, dst, rs.Checksum, proxyServer)
}

// Latest downloads the latest available version of a package.
func Latest(name, dir string, rm client.RepoMap, archs []string, proxyServer string) (string, error) {
	ver, repo, arch, err := client.FindRepoLatest(goolib.PackageInfo{name, "", ""}, rm, archs)
	if err != nil {
		return "", err
	}
	rs, err := client.FindRepoSpec(goolib.PackageInfo{name, arch, ver}, rm[repo])
	if err != nil {
		return "", err
	}
	return FromRepo(rs, repo, dir, proxyServer)
}

func download(r io.Reader, p, chksum string, proxyServer string) (err error) {
	f, err := oswrap.Create(p)
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

	logger.Infof("Successfully downloaded %s", humanize.IBytes(uint64(b)))

	if chksum != "" && hex.EncodeToString(hash.Sum(nil)) != chksum {
		return errors.New("checksum of downloaded file does not match expected checksum")
	}
	return nil
}

// ExtractPkg takes a path to a package and extracts it to a directory based on the
// package name, it returns the path to the extraced directory.
func ExtractPkg(src string) (dst string, err error) {
	dst = strings.TrimSuffix(src, filepath.Ext(src))
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

		path := filepath.Join(dst, header.Name)
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
