package clean

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/settings"
)

func TestCleanPackages(t *testing.T) {
	rootDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(rootDir)
	settings.Initialize(rootDir, false)

	cache := settings.CacheDir()
	wantFile := filepath.Join(cache, "want")
	notWantFile := filepath.Join(cache, "notWant")

	if err := oswrap.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(wantFile, nil, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(notWantFile, nil, 0700); err != nil {
		t.Fatal(err)
	}

	state := client.GooGetState{
		{LocalPath: wantFile, PackageSpec: &goolib.PkgSpec{Name: "want"}},
		{LocalPath: notWantFile, PackageSpec: &goolib.PkgSpec{Name: "notWant"}},
	}
	cleanInstalled(state, map[string]bool{"notWant": true})

	if _, err := oswrap.Stat(wantFile); err != nil {
		t.Errorf("cleanPackages removed wantDir, Stat err: %v", err)
	}

	if _, err := oswrap.Stat(notWantFile); err == nil {
		t.Errorf("cleanPackages did not remove notWantDir")
	}
}

// TestCleanDirectoryRemovesCacheEntries checks that clean removes repo index
// cache files, their .url files and unpack directories, keeps excluded
// entries, and leaves no renamed copies behind.
func TestCleanDirectoryRemovesCacheEntries(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	cache := settings.CacheDir()
	if err := os.MkdirAll(filepath.Join(cache, "pkg.x86_64.1.0.0", "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"0123abcd.rs":                    "[]",
		"0123abcd.url":                   "https://example.com/repo",
		"pkg.x86_64.1.0.0.goo":           "goo",
		"keep.x86_64.2.0.0.goo":          "keep",
		"pkg.x86_64.1.0.0/sub/file.txt":  "unpacked",
		"pkg.x86_64.1.0.0/installer.ps1": "unpacked",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(cache, filepath.FromSlash(name)), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(cache, "keep.x86_64.2.0.0.goo")
	if err := cleanDirectory(map[string]bool{keep: true}); err != nil {
		t.Fatalf("cleanDirectory: %v", err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != 1 || got[0] != "keep.x86_64.2.0.0.goo" {
		t.Errorf("cache after cleanDirectory holds %q, want only the excluded package", got)
	}
}

func TestRemoveCacheEntry(t *testing.T) {
	cache := t.TempDir()
	dir := filepath.Join(cache, "unpacked")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cache, "x.rs")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, file, filepath.Join(cache, "missing"), ""} {
		if err := removeCacheEntry(p); err != nil {
			t.Errorf("removeCacheEntry(%q) = %v, want nil", p, err)
		}
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Errorf("cache after removeCacheEntry holds %v (error %v), want nothing", entries, err)
	}
}
