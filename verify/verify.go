/*
Copyright 2018 Google Inc. All Rights Reserved.
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

// Package verify handles verifying of googet packages.
package verify

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/download"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

const (
	httpOK = 200
)

func extractVerify(r io.Reader, verify, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("verify command %q not found in package", verify)
		}
		if err != nil {
			return err
		}
		if filepath.Clean(header.Name) != filepath.Clean(verify) {
			continue
		}

		if err := oswrap.MkdirAll(dir, 0755); err != nil {
			return err
		}
		f, err := oswrap.OpenFile(filepath.Join(dir, verify), os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
}

// Files compares the checksum of all files that got installed from the package,
// returning true if all files match.
func Files(ps client.PackageState) (bool, error) {
	if len(ps.InstalledFiles) == 0 {
		return true, nil
	}
	pkg := fmt.Sprintf("%s.%s.%s", ps.PackageSpec.Name, ps.PackageSpec.Arch, ps.PackageSpec.Version)
	logger.Infof("Running file verification for %s", pkg)
	fmt.Printf("Running file verification for %s...\n", pkg)
	for file, wantChksm := range ps.InstalledFiles {
		f, err := os.Open(file)
		if os.IsNotExist(err) {
			logger.Errorf("%q: verify file %q failed, file does not exist", pkg, file)
			return false, nil
		}
		if err != nil {
			return false, err
		}
		chksm := goolib.Checksum(f)
		f.Close()
		if wantChksm != chksm {
			logger.Errorf("%q: verify file %q failed, checksum does not match", pkg, file)
			return false, nil
		}
	}
	return true, nil
}

// Command runs a packages verify command.
// Will only return true if the verify command exits with 0 or an approved
// return code.
func Command(ctx context.Context, ps client.PackageState, proxyServer string) (bool, error) {
	if ps.PackageSpec.Verify.Path == "" {
		return true, nil
	}
	pkg := fmt.Sprintf("%s.%s.%s", ps.PackageSpec.Name, ps.PackageSpec.Arch, ps.PackageSpec.Version)
	logger.Infof("Running verification command for %s", pkg)
	fmt.Printf("Running verification command for %s...\n", pkg)
	f, err := os.Open(ps.LocalPath)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	var rd bool
	if os.IsNotExist(err) {
		logger.Infof("Local package does not exist for %s, pulling from repo...", pkg)
		rd = true
	}
	// Force redownload if checksum does not match.
	// If checksum is empty this was a local install so ignore.
	if !rd && ps.Checksum != "" && goolib.Checksum(f) != ps.Checksum {
		logger.Info("Local package checksum does not match, pulling from repo...")
		rd = true
	}

	dir := strings.TrimSuffix(ps.LocalPath, filepath.Ext(ps.LocalPath))
	var r io.Reader
	r = f
	if rd {
		if ps.DownloadURL == "" {
			return false, fmt.Errorf("can not pull package %s from repo, DownloadURL not saved", pkg)
		}

		httpClient := &http.Client{}
		if proxyServer != "" {
			proxyURL, err := url.Parse(proxyServer)
			if err != nil {
				return false, err
			}
			httpClient.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		}
		resp, err := httpClient.Get(ps.DownloadURL)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != httpOK {
			return false, fmt.Errorf("Invalid return code from server, got: %d, want: %d", resp.StatusCode, httpOK)
		}
		r = resp.Body
	}
	if err := extractVerify(r, ps.PackageSpec.Verify.Path, dir); err != nil {
		return false, err
	}
	f.Close()

	// Try just running the extracted command, rextract the full package on any error.
	if err := system.Verify(dir, ps.PackageSpec); err == nil {
		return true, nil
	}

	if rd {
		if err := download.Package(ctx, ps.DownloadURL, ps.LocalPath, ps.Checksum, proxyServer); err != nil {
			return false, fmt.Errorf("error redownloading package: %v", err)
		}
	}

	dir, err = download.ExtractPkg(ps.LocalPath)
	if err != nil {
		return false, err
	}

	// Any error is deemed a verification failure.
	if err := system.Verify(dir, ps.PackageSpec); err != nil {
		logger.Errorf("%q: verify command failed: %v", pkg, err)
		return false, nil
	}
	return true, nil
}
