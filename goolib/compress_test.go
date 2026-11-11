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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/klauspost/compress/zstd"
)

var (
	gzipMagic = []byte{0x1f, 0x8b}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

type tarEntry struct {
	Name string
	Body string
}

var testEntries = []tarEntry{
	{Name: "foo/bar.txt", Body: "hello world"},
	{Name: "foo/empty", Body: ""},
	{Name: "baz", Body: strings.Repeat("compressible ", 1000)},
}

// writeTar writes entries as a tar archive to w.
func writeTar(t *testing.T, w io.Writer, entries []tarEntry) {
	t.Helper()
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.Name, Mode: 0644, Size: int64(len(e.Body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.Body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// compressedTar returns entries as a tar archive compressed with format.
func compressedTar(t *testing.T, format string, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewPackageWriter(&buf, format)
	if err != nil {
		t.Fatalf("NewPackageWriter(%q): %v", format, err)
	}
	writeTar(t, w, entries)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// readTar decompresses b with NewPackageReader and reads the tar archive in it
// to EOF.
func readTar(b []byte) ([]tarEntry, error) {
	rc, err := NewPackageReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var got []tarEntry
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return got, err
		}
		got = append(got, tarEntry{Name: h.Name, Body: string(body)})
	}
}

// randomBytes returns n pseudo-random, incompressible bytes.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

func TestPackageRoundTrip(t *testing.T) {
	for _, format := range CompressionFormats() {
		t.Run(format, func(t *testing.T) {
			got, err := readTar(compressedTar(t, format, testEntries))
			if err != nil {
				t.Fatalf("reading %s package: %v", format, err)
			}
			if diff := cmp.Diff(testEntries, got); diff != "" {
				t.Errorf("round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPackageWriterMagic(t *testing.T) {
	for _, tc := range []struct {
		format string
		magic  []byte
	}{
		{CompressionGzip, gzipMagic},
		{CompressionZstd, zstdMagic},
	} {
		t.Run(tc.format, func(t *testing.T) {
			b := compressedTar(t, tc.format, testEntries)
			if !bytes.HasPrefix(b, tc.magic) {
				t.Errorf("NewPackageWriter(%q) output starts with % x, want prefix % x", tc.format, b[:min(len(b), 4)], tc.magic)
			}
		})
	}
}

func TestPackageReaderSkippableFrame(t *testing.T) {
	zb := compressedTar(t, CompressionZstd, testEntries)
	payload := []byte("skippable frame payload")
	// Every magic number in the skippable frame range 0x184D2A50 to 0x184D2A5F.
	for low := byte(0x50); low <= 0x5f; low++ {
		var b []byte
		b = append(b, low, 0x2a, 0x4d, 0x18)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
		b = append(b, payload...)
		b = append(b, zb...)
		got, err := readTar(b)
		if err != nil {
			t.Errorf("skippable frame %#x: reading package: %v", low, err)
			continue
		}
		if diff := cmp.Diff(testEntries, got); diff != "" {
			t.Errorf("skippable frame %#x: mismatch (-want +got):\n%s", low, diff)
		}
	}
}

func TestPackageReaderPreservesGzipErrors(t *testing.T) {
	var rawTar bytes.Buffer
	writeTar(t, &rawTar, testEntries)
	for _, tc := range []struct {
		name  string
		input []byte
		want  error
	}{
		{"empty", nil, io.EOF},
		{"one byte", []byte{0x28}, io.ErrUnexpectedEOF},
		{"three bytes of zstd magic", []byte{0x28, 0xb5, 0x2f}, io.ErrUnexpectedEOF},
		{"gzip magic only", gzipMagic, io.ErrUnexpectedEOF},
		{"near-miss zstd magic", []byte("\x28\xb5\x2f\xfeplus some trailing bytes"), gzip.ErrHeader},
		{"below skippable range", []byte("\x4f\x2a\x4d\x18plus some trailing bytes"), gzip.ErrHeader},
		{"above skippable range", []byte("\x60\x2a\x4d\x18plus some trailing bytes"), gzip.ErrHeader},
		{"plain text", []byte("this is not a package at all"), gzip.ErrHeader},
		{"uncompressed tar", rawTar.Bytes(), gzip.ErrHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, gzErr := gzip.NewReader(bytes.NewReader(tc.input))
			if !errors.Is(gzErr, tc.want) {
				t.Fatalf("gzip.NewReader error = %v, want %v", gzErr, tc.want)
			}
			rc, err := NewPackageReader(bytes.NewReader(tc.input))
			if !errors.Is(err, tc.want) {
				t.Errorf("NewPackageReader error = %v, want %v (as gzip.NewReader)", err, tc.want)
			}
			if rc != nil {
				t.Errorf("NewPackageReader returned a non-nil reader with error %v", err)
			}
		})
	}
}

// errReader returns err from Read. If once is set, it returns io.EOF after
// the first call, as a reader with a transient failure would.
type errReader struct {
	err   error
	once  bool
	calls int
}

func (r *errReader) Read([]byte) (int, error) {
	r.calls++
	if r.once && r.calls > 1 {
		return 0, io.EOF
	}
	return 0, r.err
}

func TestPackageReaderSourceError(t *testing.T) {
	sentinel := errors.New("sentinel read error")
	for _, tc := range []struct {
		name string
		r    *errReader
	}{
		{"persistent", &errReader{err: sentinel}},
		{"transient", &errReader{err: sentinel, once: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPackageReader(tc.r); !errors.Is(err, sentinel) {
				t.Errorf("NewPackageReader error = %v, want %v", err, sentinel)
			}
		})
	}
}

func TestPackageReaderCorruptZstd(t *testing.T) {
	big := []tarEntry{{Name: "random", Body: string(randomBytes(1 << 20))}}
	valid := compressedTar(t, CompressionZstd, big)
	for _, tc := range []struct {
		name  string
		input []byte
	}{
		{"magic then garbage", append(append([]byte{}, zstdMagic...), randomBytes(1024)...)},
		{"magic only", zstdMagic},
		{"truncated mid stream", valid[:len(valid)/2]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The zstd decoder may report errors at construction or on Read.
			if _, err := readTar(tc.input); err == nil {
				t.Error("reading corrupt zstd package succeeded, want error")
			}
		})
	}
}

// zstdRawFrame returns a zstd frame with the given Window_Descriptor and a
// single raw block holding payload (RFC 8878, section 3.1.1).
func zstdRawFrame(windowDescriptor byte, payload []byte) []byte {
	// Frame_Header_Descriptor 0: no Frame_Content_Size, not single segment, no
	// checksum and no dictionary, so the Window_Descriptor byte follows.
	b := append(append([]byte{}, zstdMagic...), 0x00, windowDescriptor)
	// Block_Header: Last_Block set, Block_Type Raw, Block_Size len(payload).
	h := uint32(len(payload))<<3 | 1
	b = append(b, byte(h), byte(h>>8), byte(h>>16))
	return append(b, payload...)
}

func TestPackageReaderWindowCap(t *testing.T) {
	// The cap is part of googet's contract with package builders (it matches
	// the zstd CLI's default), so pin it rather than deriving cases from it.
	if maxZstdWindow != 128<<20 {
		t.Fatalf("maxZstdWindow = %d, want %d", maxZstdWindow, 128<<20)
	}
	// Window_Descriptor holds Exponent in the high 5 bits and Mantissa in the
	// low 3 bits, with windowSize = 2^(10+Exponent) * (1 + Mantissa/8).
	const atCap = 0x88 // 2^27 = 128 MiB.
	payload := []byte("window cap payload")

	t.Run("at cap", func(t *testing.T) {
		rc, err := NewPackageReader(bytes.NewReader(zstdRawFrame(atCap, payload)))
		if err != nil {
			t.Fatalf("NewPackageReader: %v", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("reading frame with a %d byte window: %v", maxZstdWindow, err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("got %q, want %q", got, payload)
		}
	})

	for _, tc := range []struct {
		name             string
		windowDescriptor byte
	}{
		{"144 MiB, just above cap", 0x89},
		{"256 MiB", 0x90},
		// The cap must stay below the library default of zstd.MaxWindowSize.
		{"512 MiB library default", 0x98},
		{"1 GiB", 0xa0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			rc, err := NewPackageReader(bytes.NewReader(zstdRawFrame(tc.windowDescriptor, payload)))
			if err == nil {
				_, err = io.ReadAll(rc)
				rc.Close()
			}
			runtime.ReadMemStats(&after)
			if !errors.Is(err, zstd.ErrWindowSizeExceeded) {
				t.Errorf("reading oversized window frame: error = %v, want %v", err, zstd.ErrWindowSizeExceeded)
			}
			if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
				t.Errorf("rejecting oversized window frame allocated %d bytes, want at most %d", alloc, 16<<20)
			}
		})
	}
}

func TestPackageWriterUnknownFormat(t *testing.T) {
	for _, format := range []string{"", "xz", "ZSTD", "Gzip", "gzip ", " zstd"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			w, err := NewPackageWriter(&buf, format)
			if err == nil {
				t.Fatalf("NewPackageWriter(%q) succeeded, want error", format)
			}
			if w != nil {
				t.Errorf("NewPackageWriter(%q) returned a non-nil writer with error %v", format, err)
			}
			for _, allowed := range CompressionFormats() {
				if !strings.Contains(err.Error(), allowed) {
					t.Errorf("NewPackageWriter(%q) error %q does not name allowed format %q", format, err, allowed)
				}
			}
			if buf.Len() != 0 {
				t.Errorf("NewPackageWriter(%q) wrote %d bytes, want 0", format, buf.Len())
			}
			if err := ValidateCompression(format); err == nil {
				t.Errorf("ValidateCompression(%q) succeeded, want error", format)
			}
		})
	}
	for _, format := range CompressionFormats() {
		if err := ValidateCompression(format); err != nil {
			t.Errorf("ValidateCompression(%q) = %v, want nil", format, err)
		}
	}
}

// closeRecorder is an io.ReadCloser that counts calls to Close.
type closeRecorder struct {
	io.Reader
	closes int
}

func (c *closeRecorder) Close() error {
	c.closes++
	return nil
}

func TestPackageReaderClose(t *testing.T) {
	// The library's default decoder concurrency is min(4, GOMAXPROCS), so on a
	// single-CPU runner dropping WithDecoderConcurrency(1) would go unnoticed.
	// Force GOMAXPROCS > 1 to keep the goroutine check meaningful.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	for _, format := range CompressionFormats() {
		t.Run(format, func(t *testing.T) {
			// Use enough incompressible data that an asynchronous decoder
			// could not finish before the first Read returns.
			big := []tarEntry{{Name: "random", Body: string(randomBytes(1 << 20))}}
			src := &closeRecorder{Reader: bytes.NewReader(compressedTar(t, format, big))}
			goroutines := runtime.NumGoroutine()
			rc, err := NewPackageReader(src)
			if err != nil {
				t.Fatalf("NewPackageReader: %v", err)
			}
			// gooserve reads many packages at once, so the decoder must not
			// start goroutines of its own.
			if _, err := rc.Read(make([]byte, 1)); err != nil {
				t.Fatalf("reading package: %v", err)
			}
			if n := runtime.NumGoroutine(); n > goroutines {
				t.Errorf("reader started %d goroutines, want 0", n-goroutines)
			}
			if _, err := io.Copy(io.Discard, rc); err != nil {
				t.Fatalf("reading package: %v", err)
			}
			for i := 0; i < 2; i++ {
				if err := rc.Close(); err != nil {
					t.Errorf("Close call %d = %v, want nil", i+1, err)
				}
			}
			if src.closes != 0 {
				t.Errorf("source closed %d times, want 0", src.closes)
			}
		})
	}
}

func TestExtractPkgSpecCompressed(t *testing.T) {
	want := &PkgSpec{Name: "test", Version: "1.2.3@4", Arch: "noarch"}
	for _, format := range CompressionFormats() {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			w, err := NewPackageWriter(&buf, format)
			if err != nil {
				t.Fatal(err)
			}
			tw := tar.NewWriter(w)
			if err := tw.WriteHeader(&tar.Header{Name: "payload", Mode: 0644, Size: 4}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("data")); err != nil {
				t.Fatal(err)
			}
			if err := WritePackageSpec(tw, want); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := ExtractPkgSpec(&buf)
			if err != nil {
				t.Fatalf("ExtractPkgSpec: %v", err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("ExtractPkgSpec mismatch (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("zstd without spec", func(t *testing.T) {
		b := compressedTar(t, CompressionZstd, testEntries)
		_, err := ExtractPkgSpec(bytes.NewReader(b))
		if err == nil || !strings.Contains(err.Error(), "no file with suffix") {
			t.Errorf("ExtractPkgSpec error = %v, want 'no file with suffix' error", err)
		}
	})

	t.Run("corrupt zstd", func(t *testing.T) {
		b := append(append([]byte{}, zstdMagic...), randomBytes(64)...)
		if _, err := ExtractPkgSpec(bytes.NewReader(b)); err == nil {
			t.Error("ExtractPkgSpec on corrupt zstd succeeded, want error")
		}
	})
}

func TestPackageWriterZstdReproducible(t *testing.T) {
	data := append(randomBytes(1<<20), bytes.Repeat([]byte("repeated "), 1<<17)...)
	var first []byte
	// Different write sizes must not change the encoded bytes.
	for _, chunk := range []int{len(data), 4096, 1000} {
		var buf bytes.Buffer
		w, err := NewPackageWriter(&buf, CompressionZstd)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(data); i += chunk {
			if _, err := w.Write(data[i:min(i+chunk, len(data))]); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = buf.Bytes()
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Errorf("zstd output with %d byte writes differs from a single write", chunk)
		}
	}
}

func TestPackageReaderMultiMemberGzip(t *testing.T) {
	var raw bytes.Buffer
	writeTar(t, &raw, testEntries)
	half := raw.Len() / 2
	// Split the tar archive across two concatenated gzip members.
	var b bytes.Buffer
	for _, part := range [][]byte{raw.Bytes()[:half], raw.Bytes()[half:]} {
		gw := gzip.NewWriter(&b)
		if _, err := gw.Write(part); err != nil {
			t.Fatal(err)
		}
		if err := gw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readTar(b.Bytes())
	if err != nil {
		t.Fatalf("reading multi-member gzip package: %v", err)
	}
	if diff := cmp.Diff(testEntries, got); diff != "" {
		t.Errorf("multi-member gzip mismatch (-want +got):\n%s", diff)
	}
}

// zstdSingleSegmentFrame returns a zstd frame with Single_Segment set, so the
// decoder derives the window size from the declared Frame_Content_Size.
func zstdSingleSegmentFrame(fcs uint64, payload []byte) []byte {
	// Frame_Header_Descriptor 0xe0: 8-byte Frame_Content_Size, Single_Segment.
	b := append(append([]byte{}, zstdMagic...), 0xe0)
	b = binary.LittleEndian.AppendUint64(b, fcs)
	// Block_Header: Last_Block set, Block_Type Raw, Block_Size len(payload).
	h := uint32(len(payload))<<3 | 1
	b = append(b, byte(h), byte(h>>8), byte(h>>16))
	return append(b, payload...)
}

func TestPackageReaderSingleSegmentWindowCap(t *testing.T) {
	payload := []byte("single segment payload")
	for _, tc := range []struct {
		name string
		fcs  uint64
	}{
		{"just above cap", 128<<20 + 1},
		{"1 GiB", 1 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			rc, err := NewPackageReader(bytes.NewReader(zstdSingleSegmentFrame(tc.fcs, payload)))
			if err == nil {
				_, err = io.ReadAll(rc)
				rc.Close()
			}
			runtime.ReadMemStats(&after)
			if !errors.Is(err, zstd.ErrDecoderSizeExceeded) && !errors.Is(err, zstd.ErrWindowSizeExceeded) {
				t.Errorf("reading frame with content size %d: error = %v, want %v or %v", tc.fcs, err, zstd.ErrDecoderSizeExceeded, zstd.ErrWindowSizeExceeded)
			}
			if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
				t.Errorf("rejecting oversized frame allocated %d bytes, want at most %d", alloc, 16<<20)
			}
		})
	}
}

func TestPackageWriterZstdLevel(t *testing.T) {
	data := bytes.Repeat([]byte("goopack zstd level fixture "), 1<<14)
	var got bytes.Buffer
	w, err := NewPackageWriter(&got, CompressionZstd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	enc, err := zstd.NewWriter(&want, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(zstdWindow))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	// The encoder is deterministic, so identical bytes pin the level; the window
	// is pinned separately because the reference encoder uses zstdWindow too.
	if zstdWindow != 8<<20 {
		t.Errorf("zstdWindow = %d, want %d", zstdWindow, 8<<20)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Errorf("NewPackageWriter(zstd) output (%d bytes) differs from SpeedBestCompression with an %d byte window (%d bytes)", got.Len(), zstdWindow, want.Len())
	}
}

func TestCompressionFormatsIsCopy(t *testing.T) {
	// Restore the list if the copy guarantee regresses, so only this test fails.
	orig := slices.Clone(compressionFormats)
	t.Cleanup(func() { compressionFormats = orig })
	formats := CompressionFormats()
	formats[0] = "xz"
	if err := ValidateCompression("xz"); err == nil {
		t.Error("ValidateCompression(\"xz\") succeeded after modifying the CompressionFormats result, want error")
	}
	if got := CompressionFormats(); got[0] != CompressionGzip {
		t.Errorf("CompressionFormats()[0] = %q after modifying an earlier result, want %q", got[0], CompressionGzip)
	}
}
