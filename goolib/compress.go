/*
Copyright 2026 Google Inc. All Rights Reserved.
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

package goolib

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
)

// Compression formats supported for GooGet packages.
const (
	// CompressionGzip is the default package compression format.
	CompressionGzip = "gzip"
	// CompressionZstd is the Zstandard (RFC 8878) package compression format.
	CompressionZstd = "zstd"
)

// CompressionFormats lists the accepted package compression formats.
var CompressionFormats = []string{CompressionGzip, CompressionZstd}

// maxZstdWindow caps the window size a zstd package may declare. Packages can
// come from untrusted sources (local files and gooserve directory scans are not
// checksum-gated), and the decoder allocates a buffer the size of the declared
// window, so the library default of 512 MiB is too generous. 128 MiB matches
// the reference zstd CLI's default decompression limit (zstd --long uses
// windowLog 27), and is well above zstdWindow.
const maxZstdWindow = 128 << 20

// zstdWindow is the window NewPackageWriter encodes with. It must not exceed
// maxZstdWindow.
const zstdWindow = 8 << 20

// ValidateCompression returns an error if format is not one of
// CompressionFormats. Matching is exact and case-sensitive.
func ValidateCompression(format string) error {
	if slices.Contains(CompressionFormats, format) {
		return nil
	}
	return fmt.Errorf("unsupported compression %q, must be one of %q", format, CompressionFormats)
}

// isZstd reports whether b starts with a zstd frame magic number or a zstd
// skippable frame magic number (0x184D2A50 to 0x184D2A5F, little-endian).
func isZstd(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	if b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd {
		return true
	}
	return b[0]&0xf0 == 0x50 && b[1] == 0x2a && b[2] == 0x4d && b[3] == 0x18
}

// NewPackageReader returns a reader that decompresses the GooGet package in r.
// It detects zstd from the leading magic bytes and treats anything else as
// gzip, so input that is not zstd fails exactly as gzip.NewReader would.
// Errors in zstd input are reported by Read rather than by NewPackageReader.
// Close releases decoder resources and never closes r.
func NewPackageReader(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(4)
	if err != nil && err != io.EOF {
		// bufio clears a read error once it is returned, so report it here
		// rather than let gzip.NewReader see a clean EOF instead.
		return nil, err
	}
	if isZstd(magic) {
		dec, err := zstd.NewReader(br,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxWindow(maxZstdWindow))
		if err != nil {
			return nil, err
		}
		return dec.IOReadCloser(), nil
	}
	// Short input is not zstd; gzip.NewReader reports the same error for it
	// as it always has.
	zr, err := gzip.NewReader(br)
	if err != nil {
		// Return an untyped nil so callers can compare the reader to nil.
		return nil, err
	}
	return zr, nil
}

// NewPackageWriter returns a writer that compresses a GooGet package to w
// using format, which must be one of CompressionFormats. The caller must Close
// the writer to flush it. Close does not close w.
func NewPackageWriter(w io.Writer, format string) (io.WriteCloser, error) {
	switch format {
	case CompressionGzip:
		return gzip.NewWriter(w), nil
	case CompressionZstd:
		// Packages are built once and installed many times, and decoding speed
		// does not depend on the level, so favour size. The library's default
		// level stores incompressible-looking blocks raw and can come out larger
		// than gzip.
		return zstd.NewWriter(w,
			zstd.WithEncoderLevel(zstd.SpeedBestCompression),
			zstd.WithWindowSize(zstdWindow))
	}
	return nil, ValidateCompression(format)
}
