package settings_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/settings"
)

func TestInitialize(t *testing.T) {
	rootDir := t.TempDir()
	f, err := os.Create(filepath.Join(rootDir, "googet.conf"))
	if err != nil {
		t.Fatalf("error creating conf file: %v", err)
	}
	content := []byte("archs: [noarch, x86_64, arm64]\ncachelife: 10m\nallowunsafeurl: true")
	if _, err := f.Write(content); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error closing conf file: %v", err)
	}

	settings.Initialize(rootDir, true)

	if got, want := settings.Confirm, true; got != want {
		t.Errorf("settings.Confirm got: %v, want: %v", got, want)
	}

	wantArches := []string{"noarch", "x86_64", "arm64"}
	if diff := cmp.Diff(wantArches, settings.Archs); diff != "" {
		t.Errorf("settings.Archs unexpected diff (-want +got):\n%v", diff)
	}

	if got, want := settings.CacheLife, 10*time.Minute; got != want {
		t.Errorf("settings.CacheLife got: %v, want: %v", got, want)
	}

	if got, want := settings.AllowUnsafeURL, true; got != want {
		t.Errorf("settings.AllowUnsafeURL got: %v, want: %v", got, want)
	}
}
