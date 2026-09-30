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
	"hash"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// Package downloads a package from the given url,
// the provided SHA256 checksum will be checked during download.
func Package(ctx context.Context, pkgURL, dst, chksum string, downloader *client.Downloader) error {

	isGCSURL, bucket, object := goolib.SplitGCSUrl(pkgURL)
	if isGCSURL {
		if err := oswrap.RemoveAll(dst); err != nil {
			return err
		}
		return packageGCS(ctx, bucket, object, dst, chksum, stallTimeout(downloader))
	}

	return packageHTTP(ctx, pkgURL, dst, chksum, downloader)
}

// stallTimeout returns the stall timeout configured on downloader. Zero, which
// is also returned for a nil downloader, means client.DefaultStallTimeout.
func stallTimeout(downloader *client.Downloader) time.Duration {
	if downloader == nil {
		return 0
	}
	return downloader.StallTimeout
}

const (
	// maxNoProgressRetries is the number of consecutive retries allowed after
	// attempts that did not advance the download past its previous high-water
	// mark. The counter resets whenever an attempt makes forward progress.
	maxNoProgressRetries = 3
	// minAttemptProgress is the forward progress, in bytes, that a failed
	// attempt must make to be exempt from maxLowProgressAttempts.
	minAttemptProgress = 1 << 20
	// maxLowProgressAttempts caps the failed attempts that each advanced the
	// download by less than minAttemptProgress, so that a connection that
	// repeatedly delivers a few bytes and then fails cannot loop forever.
	// Attempts that make at least minAttemptProgress never count toward it, so
	// a slow or flaky link that keeps making progress is never abandoned.
	maxLowProgressAttempts = 20
	// baseBackoff is the delay before the first retry.
	baseBackoff = time.Second
	// maxBackoff caps the exponential backoff delay before jitter is applied.
	maxBackoff = 30 * time.Second
	// backoffJitter is the maximum relative jitter applied to each delay.
	backoffJitter = 0.2
)

// sleep waits for d or until ctx is done, whichever happens first. It is a
// package variable so that tests can avoid real delays.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffDelay returns the delay before the next retry given the number of
// consecutive attempts that made no progress. The sequence is 1s, 1s, 2s, 4s,
// and so on, capped at maxBackoff, with +/- backoffJitter applied.
func backoffDelay(noProgress int) time.Duration {
	shift := noProgress - 1
	if shift < 0 {
		shift = 0
	}
	d := maxBackoff
	if shift < 6 {
		if b := baseBackoff << shift; b < maxBackoff {
			d = b
		}
	}
	jitter := 1 + backoffJitter*(2*rand.Float64()-1)
	return time.Duration(float64(d) * jitter)
}

// opener opens a stream of the object starting at offset. It returns the
// stream, the offset the stream actually starts at (which is either offset or
// 0 when the source ignored the resume request), and the total size of the
// object in bytes (or -1 if unknown). It returns an error wrapping
// errResumeRejected when the source cannot serve offset.
type opener func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error)

// errResumeRejected reports that the source cannot serve the requested resume
// offset, so the partial file must be discarded and the download restarted
// from the beginning.
var errResumeRejected = errors.New("source cannot resume at the requested offset")

// statusError reports a non-successful HTTP status from a download request.
type statusError struct {
	code   int
	status string
}

// Error implements the error interface.
func (e *statusError) Error() string {
	return "unexpected HTTP status " + e.status
}

// checksumError reports that a completed download does not match its
// expected SHA256 checksum.
type checksumError struct {
	got, want string
}

// Error implements the error interface.
func (e *checksumError) Error() string {
	return fmt.Sprintf("checksum doesn't match: got %s, want %s", e.got, e.want)
}

// writeError wraps an error returned while writing to the destination file so
// that disk errors are never mistaken for retryable network errors.
type writeError struct {
	err error
}

// Error implements the error interface.
func (e *writeError) Error() string {
	return "writing download to disk: " + e.err.Error()
}

// Unwrap returns the underlying write error.
func (e *writeError) Unwrap() error {
	return e.err
}

// fileWriter wraps writer errors in writeError.
type fileWriter struct {
	w io.Writer
}

// Write implements io.Writer.
func (fw fileWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err != nil {
		err = &writeError{err: err}
	}
	return n, err
}

// isRetryable reports whether err from a download attempt is transient and
// may succeed on a subsequent attempt. Errors are never retryable once the
// parent context is done.
func isRetryable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var we *writeError
	if errors.As(err, &we) {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusTooManyRequests
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code >= 500 || ge.Code == http.StatusTooManyRequests
	}
	if errors.Is(err, client.ErrDownloadStalled) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// HTTP/2 stream and GOAWAY errors are not exported as stable types, and
	// Windows socket errors (WSAECONNRESET) do not match the syscall constants
	// above, so fall back to matching well-known error text.
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"stream error", "goaway", "connection reset", "forcibly closed", "broken pipe"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// retryBudget tracks forward progress across failed attempts and decides when
// a download should be abandoned. Only a true stall exhausts it: attempts that
// make at least minAttemptProgress are always retried.
type retryBudget struct {
	// highWater is the largest offset any attempt has reached since the
	// download last restarted from byte 0.
	highWater int64
	// noProgress counts consecutive failed attempts that did not raise
	// highWater.
	noProgress int
	// lowProgress counts failed attempts that raised highWater by less than
	// minAttemptProgress, including those that did not raise it at all.
	lowProgress int
	// peak is the largest offset reached before any restart from byte 0.
	peak int64
	// stuckRestarts counts consecutive restarts from byte 0 whose preceding
	// run did not get past peak.
	stuckRestarts int
}

// record accounts for a failed attempt that reached offset end. It returns a
// non-nil error when the budget is exhausted.
func (b *retryBudget) record(end int64) error {
	advance := end - b.highWater
	if advance > 0 {
		b.highWater = end
		b.noProgress = 0
	} else {
		advance = 0
		b.noProgress++
	}
	if advance < minAttemptProgress {
		b.lowProgress++
	}
	if b.noProgress > maxNoProgressRetries {
		return fmt.Errorf("retries exhausted after %d consecutive attempts without progress", b.noProgress)
	}
	if b.lowProgress >= maxLowProgressAttempts {
		return fmt.Errorf("retries exhausted after %d attempts that each made less than %s of progress", b.lowProgress, humanize.IBytes(minAttemptProgress))
	}
	return nil
}

// rebase resets progress tracking after the partial file was discarded. A
// restart from byte 0 is a fresh download, so offsets reached before the
// restart must not make later attempts look like they made no progress.
//
// A source can force restarts indefinitely, for example by rejecting every
// resume request or by ignoring Range and failing at the same byte each time.
// rebase therefore returns a non-nil error once more than
// maxNoProgressRetries consecutive restarts follow runs that never got past
// the largest offset reached before an earlier restart. A run that does get
// past it resets that count, so a download that keeps making progress is
// never abandoned.
func (b *retryBudget) rebase() error {
	if b.highWater > b.peak {
		b.peak = b.highWater
		b.stuckRestarts = 0
	} else {
		b.stuckRestarts++
	}
	b.highWater = 0
	b.noProgress = 0
	b.lowProgress = 0
	if b.stuckRestarts > maxNoProgressRetries {
		return fmt.Errorf("retries exhausted after %d consecutive restarts that never got past byte %d", b.stuckRestarts, b.peak)
	}
	return nil
}

// rehash computes the SHA256 hash and size of the current contents of f.
func rehash(f *os.File) (hash.Hash, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}
	return h, size, nil
}

// discardPartial truncates f so that the next attempt starts from the
// beginning.
func discardPartial(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return &writeError{err: err}
	}
	return nil
}

// alignToStream positions f and h for a stream that begins at start, given
// that f holds size bytes. A source that ignored the resume request restarts
// at 0, so the partial file is discarded and h is reset.
func alignToStream(f *os.File, h hash.Hash, name string, size, start int64) error {
	switch {
	case start == size:
		if start > 0 {
			logger.Infof("resuming download of %s at byte %d", name, start)
		}
	case start == 0:
		logger.Infof("server did not resume download of %s, restarting from start", name)
		if err := discardPartial(f); err != nil {
			return err
		}
		h.Reset()
	default:
		return fmt.Errorf("%w: source resumed at offset %d, want %d or 0", errResumeRejected, start, size)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return &writeError{err: err}
	}
	return nil
}

// streamAttempt makes one attempt to copy the object from open into f and h,
// resuming at size, the number of bytes already in f. It returns the offset
// the attempt started at, the offset it reached, and the error that ended it,
// which is nil if the stream completed.
func streamAttempt(ctx context.Context, f *os.File, h hash.Hash, name string, size int64, stallTimeout time.Duration, open opener) (int64, int64, error) {
	// A per-attempt context lets the StallReader abort a stalled stream
	// without canceling the parent context.
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	body, start, total, err := open(attemptCtx, size)
	if err != nil {
		return size, size, err
	}
	sr := client.NewStallReader(body, stallTimeout, cancel)
	defer sr.Close()

	if err := alignToStream(f, h, name, size, start); err != nil {
		return size, size, err
	}
	bar := progress.NewBar(fmt.Sprintf("Downloading %s", filepath.Base(f.Name())), total, start)
	n, err := io.Copy(io.MultiWriter(fileWriter{w: f}, h, bar), sr)
	if err != nil {
		bar.Abort()
	} else {
		bar.Finish()
	}
	return start, start + n, err
}

// verify flushes a completed download in f and checks its hash h against
// chksum. It returns a *checksumError on a mismatch.
func verify(f *os.File, h hash.Hash, dst, chksum string) error {
	if err := f.Sync(); err != nil {
		logger.Warningf("syncing %s: %v", dst, err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != chksum {
		return &checksumError{got: sum, want: chksum}
	}
	return nil
}

// fetch downloads the object provided by open into dst, verifying chksum. An
// existing partial dst is resumed when the source supports it. Transient
// failures are retried with exponential backoff until retryBudget is
// exhausted, so a download is only abandoned when it truly stops making
// progress. A source that rejects a resume offset causes the partial file to
// be discarded and the download restarted from the beginning, as does a
// checksum mismatch after a resumed attempt, at most once.
func fetch(ctx context.Context, name, dst, chksum string, stallTimeout time.Duration, open opener) error {
	// Try to open any already existing file, otherwise create new file.
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	var budget *retryBudget
	restartedAfterMismatch := false
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("downloading %s: %w", name, err)
		}

		// Re-synchronize hash and size with the file on disk.
		h, size, err := rehash(f)
		if err != nil {
			return err
		}
		if budget == nil {
			budget = &retryBudget{highWater: size}
		}

		// If the file checksum matches what we expect, then the file is already
		// downloaded and we can quit early.
		if sum := hex.EncodeToString(h.Sum(nil)); sum == chksum {
			logger.Infof("using existing file: %s (sum = %s)", dst, sum)
			return f.Close()
		}
		if attempt > 1 {
			logger.Infof("retrying download of %s (attempt %d, %d bytes on disk)", name, attempt, size)
		} else if size > 0 {
			logger.Infof("existing file size: %d", size)
		}

		start, end, attemptErr := streamAttempt(ctx, f, h, name, size, stallTimeout, open)
		if attemptErr == nil {
			err := verify(f, h, dst, chksum)
			var ce *checksumError
			if errors.As(err, &ce) && start > 0 && !restartedAfterMismatch {
				// The bytes kept from earlier attempts may belong to a different
				// version of the object, so try once more from the beginning.
				logger.Warningf("download of %s resumed at byte %d failed verification (%v), restarting from start", name, start, err)
				if err := discardPartial(f); err != nil {
					return err
				}
				if rerr := budget.rebase(); rerr != nil {
					f.Close()
					os.RemoveAll(dst) // Delete the bad file.
					return fmt.Errorf("downloading %s: %v: %w", name, rerr, err)
				}
				restartedAfterMismatch = true
				continue
			}
			if err != nil {
				f.Close()
				os.RemoveAll(dst) // Delete the bad file.
				return err
			}
			logger.Infof("Successfully downloaded %s bytes", humanize.IBytes(uint64(end)))
			return f.Close()
		}

		// Flush any partially written data to disk so the next attempt can resume.
		if err := f.Sync(); err != nil {
			logger.Warningf("syncing partial download %s: %v", dst, err)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("downloading %s: %w (last error: %v)", name, ctx.Err(), attemptErr)
		}
		restarted := false
		switch {
		case errors.Is(attemptErr, errResumeRejected):
			logger.Warningf("discarding %d partial bytes of %s and restarting from start: %v", size, name, attemptErr)
			if err := discardPartial(f); err != nil {
				return err
			}
			// A restart is not progress.
			start, end = 0, 0
			restarted = true
		case !isRetryable(ctx, attemptErr):
			return fmt.Errorf("downloading %s: %w", name, attemptErr)
		case start == 0 && size > 0:
			// The source ignored the resume request, so alignToStream discarded
			// the partial file and this attempt restarted from byte 0.
			restarted = true
		}
		if restarted {
			if err := budget.rebase(); err != nil {
				return fmt.Errorf("downloading %s: %v: %w", name, err, attemptErr)
			}
		}
		if err := budget.record(end); err != nil {
			return fmt.Errorf("downloading %s: %v: %w", name, err, attemptErr)
		}

		d := backoffDelay(budget.noProgress)
		logger.Warningf("download of %s failed after %s this attempt: %v; retrying in %v", name, humanize.IBytes(uint64(end-start)), attemptErr, d.Round(time.Millisecond))
		if err := sleep(ctx, d); err != nil {
			return fmt.Errorf("downloading %s: %w (last error: %v)", name, err, attemptErr)
		}
	}
}

// checkContentRange verifies that v, a Content-Range header value of the form
// "bytes START-END/SIZE", starts at offset. Any other value is reported as an
// error wrapping errResumeRejected.
func checkContentRange(v string, offset int64) error {
	spec, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return fmt.Errorf("%w: invalid Content-Range %q", errResumeRejected, v)
	}
	first, _, ok := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if !ok || err != nil {
		return fmt.Errorf("%w: invalid Content-Range %q", errResumeRejected, v)
	}
	if start != offset {
		return fmt.Errorf("%w: Content-Range %q does not start at requested offset %d", errResumeRejected, v, offset)
	}
	return nil
}

// httpOpener returns an opener that fetches pkgURL over HTTP(S), using a Range
// request to resume when the server supports it.
func httpOpener(pkgURL string, downloader *client.Downloader) opener {
	return func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		resume := false
		if offset > 0 {
			ok, length, err := downloader.CanResume(ctx, pkgURL)
			if err != nil {
				logger.Errorf("CanResume: %v", err)
			}
			resume = ok && offset < length
		}

		req, err := downloader.NewRequest(ctx, http.MethodGet, pkgURL, nil)
		if err != nil {
			return nil, 0, 0, err
		}
		if resume {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}

		resp, err := downloader.HTTPClient.Do(req)
		if err != nil {
			return nil, 0, 0, err
		}
		switch {
		case resp.StatusCode == http.StatusPartialContent && resume:
			if err := checkContentRange(resp.Header.Get("Content-Range"), offset); err != nil {
				resp.Body.Close()
				return nil, 0, 0, err
			}
			total := int64(-1)
			if resp.ContentLength >= 0 {
				total = offset + resp.ContentLength
			}
			return resp.Body, offset, total, nil
		case resp.StatusCode == http.StatusOK:
			// A 200 OK means the full object is being sent from byte 0, even if a
			// Range header was sent.
			return resp.Body, 0, resp.ContentLength, nil
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && resume:
			resp.Body.Close()
			return nil, 0, 0, fmt.Errorf("%w: server returned %s for offset %d", errResumeRejected, resp.Status, offset)
		default:
			resp.Body.Close()
			return nil, 0, 0, &statusError{code: resp.StatusCode, status: resp.Status}
		}
	}
}

// packageHTTP downloads a package from an HTTP(S) server.
func packageHTTP(ctx context.Context, pkgURL, dst, chksum string, downloader *client.Downloader) error {
	return fetch(ctx, strings.TrimPrefix(pkgURL, "oauth-"), dst, chksum, stallTimeout(downloader), httpOpener(pkgURL, downloader))
}

// gcsRangeOpener returns an opener that reads a Google Cloud Storage object
// from an offset to its end through newRangeReader. A range error on a resumed
// read, which the JSON and XML APIs report as HTTP 416 when the offset is at or
// beyond the end of the object (for example, because it was replaced), is
// reported as an error wrapping errResumeRejected.
func gcsRangeOpener(newRangeReader func(ctx context.Context, offset int64) (io.ReadCloser, int64, error)) opener {
	return func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		r, total, err := newRangeReader(ctx, offset)
		if err != nil {
			var ge *googleapi.Error
			if offset > 0 && errors.As(err, &ge) && ge.Code == http.StatusRequestedRangeNotSatisfiable {
				return nil, 0, 0, fmt.Errorf("%w: %v", errResumeRejected, err)
			}
			return nil, 0, 0, err
		}
		return r, offset, total, nil
	}
}

// newGCSOpener returns an opener for a Google Cloud Storage object and a
// function that releases its resources. It is a package variable so that tests
// can substitute a fake without contacting GCS.
var newGCSOpener = func(ctx context.Context, bucket, object string) (opener, func() error, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	obj := c.Bucket(bucket).Object(object)
	open := gcsRangeOpener(func(ctx context.Context, offset int64) (io.ReadCloser, int64, error) {
		r, err := obj.NewRangeReader(ctx, offset, -1)
		if err != nil {
			return nil, 0, err
		}
		return r, r.Attrs.Size, nil
	})
	return open, c.Close, nil
}

// packageGCS downloads a package from Google Cloud Storage, aborting reads
// that receive no data for stallTimeout.
func packageGCS(ctx context.Context, bucket, object string, dst, chksum string, stallTimeout time.Duration) error {
	open, closeFn, err := newGCSOpener(ctx, bucket, object)
	if err != nil {
		return err
	}
	defer closeFn()

	name := fmt.Sprintf("gs://%s/%s", bucket, object)
	logger.Infof("Downloading %s", name)
	return fetch(ctx, name, dst, chksum, stallTimeout, open)
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
	spec, repo, arch, err := client.FindRepoLatest(goolib.PackageInfo{Name: name, Arch: "", Ver: ""}, rm, archs, "", false)
	if err != nil {
		return "", "", err
	}
	rs, err := client.FindRepoSpec(goolib.PackageInfo{Name: name, Arch: arch, Ver: spec.Version}, rm[repo])
	if err != nil {
		return "", "", err
	}
	return FromRepo(ctx, rs, repo, dir, downloader)
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
