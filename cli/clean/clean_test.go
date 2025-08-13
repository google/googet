package clean

import (
	"io/ioutil"
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
