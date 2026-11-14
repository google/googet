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

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
	"github.com/google/logger"
)

func TestMain(m *testing.M) {
	logger.Init("test", false, false, io.Discard)
	os.Exit(m.Run())
}

func TestRunSyncMixedCompression(t *testing.T) {
	// t.TempDir cleanup fails on Windows if runSync leaves package files open.
	root := t.TempDir()
	pkgDir := filepath.Join(root, "packages")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Use fewer workers than packages so the concurrency limit is exercised.
	defer func(n int) { syncConcurrency = n }(syncConcurrency)
	syncConcurrency = 2
	formats := goolib.CompressionFormats()
	var want []goolib.RepoSpec
	for i := 0; i < 8; i++ {
		format := formats[i%len(formats)]
		ps := goolib.PkgSpec{Name: fmt.Sprintf("pkg%d", i), Version: "1.0.0@1", Arch: "noarch"}
		rs := testutil.GenGooCompressed(t, pkgDir, t.TempDir(), ps, format)
		rs.Source = filepath.Join(pkgDir, rs.Source)
		want = append(want, rs)
	}

	if err := runSync(context.Background(), root, "packages"); err != nil {
		t.Fatalf("runSync: %v", err)
	}
	got := repoContents.rs
	sort.Slice(got, func(i, j int) bool { return got[i].Source < got[j].Source })
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("indexed packages mismatch (-want +got):\n%s", diff)
	}
}
