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
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/dustin/go-humanize"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

// Package downloads a package from the given url,
// the provided SHA256 checksum will be checked during download.
func Package(ctx context.Context, pkgURL, dst, chksum string, downloader *client.Downloader) error {

	isGCSURL, bucket, object := goolib.SplitGCSUrl(pkgURL)
	if isGCSURL {
		if err := oswrap.RemoveAll(dst); err != nil {
			return err
		}
		return packageGCS(ctx, bucket, object, dst, chksum)
	}

	return packageHTTP(ctx, pkgURL, dst, chksum, downloader)
}

// packageHTTP downloads a package from an HTTP(S) server.
func packageHTTP(ctx context.Context, url, dst, chksum string, downloader *client.Downloader) error {
	// Try to open any already existing file, otherwise create new file.
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Hash the contents of the existing file.
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	if err != nil {
		return err
	}
	// If the file checksum matches what we expect, then the file is already
	// downloaded and we can quit early.
	if sum := hex.EncodeToString(hash.Sum(nil)); sum == chksum {
		logger.Infof("using existing file: %s (sum = %s)", dst, sum)
		return nil
	}
	// Otherwise we have either an empty or partial download.
	// Check that the server supports ranged requests and that the
	// existing file is smaller than what we want to download.
	logger.Infof("existing file size: %d", size)
	ok, length, err := downloader.CanResume(ctx, url)
	if err != nil {
		logger.Errorf("CanResume: %v", err)
	}
	req, err := downloader.NewRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if ok && size < length {
		logger.Infof("resuming download of %s (%d bytes remaining)", url, length-size)
		req.Header.Add("Range", fmt.Sprintf("bytes=%d-", size))
	} else {
		// Get rid of the old file and download from start, resetting hash.
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, 0); err != nil {
			return err
		}
		hash.Reset()
	}
	resp, err := downloader.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("downloading %s: %v", url, err)
	}
	// Continue hashing the file as we download it.
	n, err := io.Copy(io.MultiWriter(hash, f), resp.Body)
	if err != nil {
		return fmt.Errorf("downloading %s: %v", url, err)
	}
	// Verify the checksum of the fully downloaded file.
	if sum := hex.EncodeToString(hash.Sum(nil)); sum != chksum {
		os.RemoveAll(dst) // delete the bad file
		return fmt.Errorf("checksum doesn't match: got %s, want %s", sum, chksum)
	}
	logger.Infof("Successfully downloaded %s bytes", humanize.IBytes(uint64(n)))
	return nil
}

// Downloads a package from Google Cloud Storage
func packageGCS(ctx context.Context, bucket, object string, dst, chksum string) error {
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

// FromRepo downloads a package from a repo. It returns the path to the
// downloaded file and the download URL of the package.
func FromRepo(ctx context.Context, rs goolib.RepoSpec, repo, dir string, downloader *client.Downloader) (string, string, error) {
	pkgURL, err := url.JoinPath(repo, "..", rs.Source)
	if err != nil {
		return "", "", err
	}
	pn := goolib.PackageInfo{Name: rs.PackageSpec.Name, Arch: rs.PackageSpec.Arch, Ver: rs.PackageSpec.Version}.PkgName()
	dst := filepath.Join(dir, filepath.Base(pn))
	return dst, pkgURL, Package(ctx, pkgURL, dst, rs.Checksum, downloader)
}

// Latest downloads the latest available version of a package.
func Latest(ctx context.Context, name, dir string, rm client.RepoMap, archs []string, downloader *client.Downloader) (string, string, error) {
	spec, repo, arch, err := client.FindRepoLatest(goolib.PackageInfo{Name: name, Arch: "", Ver: ""}, rm, archs)
	if err != nil {
		return "", "", err
	}
	rs, err := client.FindRepoSpec(goolib.PackageInfo{Name: name, Arch: arch, Ver: spec.Version}, rm[repo])
	if err != nil {
		return "", "", err
	}
	return FromRepo(ctx, rs, repo, dir, downloader)
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
// package name, it returns the path to the extracted directory.
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
		if strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
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
