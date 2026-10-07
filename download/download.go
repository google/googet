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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/dustin/go-humanize"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/progress"
	"github.com/google/logger"
	"google.golang.org/api/googleapi"
)

// maxFailures is how many consecutive download attempts may fail without
// making progress before Package gives up.
const maxFailures = 5

// retryDelay returns how long to wait before the next attempt after the given
// number of consecutive failures.
var retryDelay = func(failures int) time.Duration {
	return time.Second << failures
}

var (
	// errChecksum reports a downloaded file that does not match its checksum.
	errChecksum = errors.New("checksum doesn't match")
	// errResumed marks a checksum mismatch in a download that was resumed from
	// a partial file, which may have been corrupt.
	errResumed = errors.New("download was resumed")
)

// statusError reports an unexpected HTTP response status.
type statusError struct {
	url    string
	status string
	code   int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("downloading %s: unexpected status %s", e.url, e.status)
}

// Package downloads a package from the given url,
// the provided SHA256 checksum will be checked during download.
// Failed or stalled transfers are retried, resuming from the partial file
// when the server supports it, until five consecutive attempts make no
// progress. A transfer that keeps receiving data is never abandoned.
func Package(ctx context.Context, pkgURL, dst, chksum string, downloader *client.Downloader) error {
	isGCSURL, bucket, object := goolib.SplitGCSUrl(pkgURL)
	attempt := func() error {
		if !isGCSURL {
			return packageHTTP(ctx, pkgURL, dst, chksum, downloader)
		}
		if err := oswrap.RemoveAll(dst); err != nil {
			return err
		}
		return packageGCS(ctx, bucket, object, dst, chksum, downloader)
	}
	// Progress is measured against the largest partial file seen, because an
	// attempt that cannot resume starts again from zero.
	best := fileSize(dst)
	failures := 0
	for {
		err := attempt()
		if err == nil || ctx.Err() != nil || !retryable(err) {
			return err
		}
		if size := fileSize(dst); size > best {
			best, failures = size, 0
		} else {
			failures++
		}
		if failures >= maxFailures {
			return fmt.Errorf("giving up after %d attempts without progress: %w", failures, err)
		}
		d := retryDelay(failures)
		logger.Warningf("Downloading %s failed, retrying in %v: %v", pkgURL, d, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

// retryable reports whether a failed download attempt may succeed if retried:
// it stalled, the connection failed, or the server reported a temporary
// error. A checksum mismatch is retried only if the download was resumed from
// a partial file, which has since been deleted.
func retryable(err error) bool {
	var se *statusError
	var ge *googleapi.Error
	var oe *net.OpError
	switch {
	case errors.Is(err, errChecksum):
		return errors.Is(err, errResumed)
	case errors.As(err, &se):
		return temporaryStatus(se.code)
	case errors.As(err, &ge):
		return temporaryStatus(ge.Code)
	}
	return errors.Is(err, client.ErrStalled) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &oe)
}

// temporaryStatus reports whether an HTTP status code may succeed if retried.
func temporaryStatus(code int) bool {
	return code >= 500 || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests
}

// fileSize returns the size of the file at path, or zero if it cannot be read.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// packageHTTP downloads a package from an HTTP(S) server.
func packageHTTP(ctx context.Context, url, dst, chksum string, downloader *client.Downloader) (err error) {
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
	ctx, sw := client.WatchStall(ctx, downloader.StallTimeout)
	defer sw.Stop(&err)
	ok, length, err := downloader.CanResume(ctx, url)
	if err != nil {
		logger.Errorf("CanResume: %v", err)
	}
	sw.Progress()
	req, err := downloader.NewRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// restart discards any partial download so the response body is written
	// from the beginning of the file.
	restart := func() error {
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, 0); err != nil {
			return err
		}
		hash.Reset()
		size = 0
		return nil
	}
	if ok && size < length {
		logger.Infof("resuming download of %s (%d bytes remaining)", url, length-size)
		req.Header.Add("Range", fmt.Sprintf("bytes=%d-", size))
	} else if err := restart(); err != nil {
		return err
	}
	resp, err := downloader.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sw.Progress()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return &statusError{url: url, status: resp.Status, code: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusOK && size > 0 {
		// The server ignored the Range header and is sending the whole file;
		// appending it to the partial download would corrupt the file and
		// overstate the progress total.
		logger.Infof("server ignored range request for %s, restarting download", url)
		if err := restart(); err != nil {
			return err
		}
	}
	resumedAt := size
	// The total is what is already on disk plus what the server will send.
	total := int64(-1)
	if resp.ContentLength >= 0 {
		total = size + resp.ContentLength
	}
	bar := progress.NewBar(fmt.Sprintf("Downloading %s", filepath.Base(dst)), total, size)
	// Continue hashing the file as we download it.
	n, err := io.Copy(io.MultiWriter(hash, f, bar), sw.Reader(resp.Body))
	if err != nil {
		bar.Abort()
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	bar.Finish()
	// Verify the checksum of the fully downloaded file.
	if sum := hex.EncodeToString(hash.Sum(nil)); sum != chksum {
		f.Close()         // Windows cannot delete an open file.
		os.RemoveAll(dst) // delete the bad file
		err := fmt.Errorf("%w: got %s, want %s", errChecksum, sum, chksum)
		if resumedAt > 0 {
			err = fmt.Errorf("%w (%w at byte %d)", err, errResumed, resumedAt)
		}
		return err
	}
	logger.Infof("Successfully downloaded %s bytes", humanize.IBytes(uint64(n)))
	return nil
}

// packageGCS downloads a package from Google Cloud Storage.
func packageGCS(ctx context.Context, bucket, object string, dst, chksum string, downloader *client.Downloader) (err error) {
	ctx, sw := client.WatchStall(ctx, downloader.StallTimeout)
	defer sw.Stop(&err)
	gcs, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}
	defer gcs.Close()

	r, err := gcs.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		return err
	}
	defer r.Close()

	logger.Infof("Downloading gs://%s/%s", bucket, object)
	return download(sw.Reader(r), r.Attrs.Size, dst, chksum)
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

// Latest downloads the latest available version of the package named by
// pi.Name. If pi.Arch is set, only that architecture is considered; otherwise
// the architectures in archs are considered in order of preference. pi.Ver is
// ignored.
func Latest(ctx context.Context, pi goolib.PackageInfo, dir string, rm client.RepoMap, archs []string, downloader *client.Downloader) (string, string, error) {
	spec, repo, arch, err := client.FindRepoLatest(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: ""}, rm, archs, "", false)
	if err != nil {
		return "", "", err
	}
	rs, err := client.FindRepoSpec(goolib.PackageInfo{Name: pi.Name, Arch: arch, Ver: spec.Version}, rm[repo])
	if err != nil {
		return "", "", err
	}
	return FromRepo(ctx, rs, repo, dir, downloader)
}

// download copies r to dst, verifying the SHA256 checksum, and renders a
// progress bar when enabled.
func download(r io.Reader, size int64, dst, chksum string) (err error) {
	f, err := oswrap.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cErr := f.Close(); cErr != nil && err == nil {
			err = cErr
		}
	}()

	bar := progress.NewBar(fmt.Sprintf("Downloading %s", filepath.Base(dst)), size, 0)
	hash := sha256.New()
	tw := io.MultiWriter(f, hash, bar)

	b, err := io.Copy(tw, r)
	if err != nil {
		bar.Abort()
		return err
	}
	bar.Finish()

	if sum := hex.EncodeToString(hash.Sum(nil)); sum != chksum {
		return fmt.Errorf("%w: got %s, want %s", errChecksum, sum, chksum)
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
		absDst, err := filepath.Abs(dst)
		if err != nil {
			return "", err
		}
		absPath := filepath.Join(absDst, name)
		if !strings.HasPrefix(absPath, absDst) {
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
