package settings_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
)

func TestInitialize(t *testing.T) {
	rootDir := t.TempDir()
	f, err := os.Create(filepath.Join(rootDir, "googet.conf"))
	if err != nil {
		t.Fatalf("error creating conf file: %v", err)
	}
	content := []byte("archs: [noarch, x86_64, arm64]\ncachelife: 10m\nlockfilemaxage: 1x\nallowunsafeurl: true\nprogress: false\ninstalltimeout: 90m\ninactivitytimeout: 2m\ninactivitymode: Enforce")
	if _, err := f.Write(content); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error closing conf file: %v", err)
	}

	origTimeout, origInactivity, origMode := goolib.Timeout, goolib.InactivityTimeout, goolib.InactivityMode
	t.Cleanup(func() {
		goolib.Timeout, goolib.InactivityTimeout, goolib.InactivityMode = origTimeout, origInactivity, origMode
	})
	settings.Initialize(rootDir, true)

	if got, want := settings.Confirm, true; got != want {
		t.Errorf("settings.Confirm got: %v, want: %v", got, want)
	}

	t.Run("Parsing Architectures", func(t *testing.T) {
		wantArches := []string{"noarch", "x86_64", "arm64"}
		if diff := cmp.Diff(wantArches, settings.Archs); diff != "" {
			t.Errorf("settings.Archs unexpected diff (-want +got):\n%v", diff)
		}
	})

	t.Run("Parsing CacheLife", func(t *testing.T) {
		wantCacheLife := 10 * time.Minute
		if got := settings.CacheLife; got != wantCacheLife {
			t.Errorf("settings.CacheLife got: %v, want: %v", got, wantCacheLife)
		}
	})

	t.Run("Parsing LockFileMaxAge", func(t *testing.T) {
		wantLockFileMaxAge := 24 * time.Hour
		if got := settings.LockFileMaxAge; got != wantLockFileMaxAge {
			t.Errorf("settings.LockFileMaxAge got: %v, want: %v", got, wantLockFileMaxAge)
		}
	})

	t.Run("Parsing AllowUnsafeURL", func(t *testing.T) {
		wantAllowUnsafeURL := true
		if got := settings.AllowUnsafeURL; got != wantAllowUnsafeURL {
			t.Errorf("settings.AllowUnsafeURL got: %v, want: %v", got, wantAllowUnsafeURL)
		}
	})

	t.Run("Parsing Progress", func(t *testing.T) {
		if got, want := settings.Progress, false; got != want {
			t.Errorf("settings.Progress got: %v, want: %v", got, want)
		}
	})

	t.Run("Parsing InstallTimeout", func(t *testing.T) {
		if got, want := goolib.Timeout, 90*time.Minute; got != want {
			t.Errorf("goolib.Timeout got: %v, want: %v", got, want)
		}
	})

	t.Run("Parsing InactivityTimeout", func(t *testing.T) {
		if got, want := goolib.InactivityTimeout, 2*time.Minute; got != want {
			t.Errorf("goolib.InactivityTimeout got: %v, want: %v", got, want)
		}
	})

	t.Run("Parsing InactivityMode", func(t *testing.T) {
		if got, want := goolib.InactivityMode, goolib.InactivityEnforce; got != want {
			t.Errorf("goolib.InactivityMode got: %q, want: %q", got, want)
		}
	})
}

func TestInvalidInactivityMode(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte("inactivitymode: kill"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	orig := goolib.InactivityMode
	t.Cleanup(func() { goolib.InactivityMode = orig })
	goolib.InactivityMode = goolib.InactivityMonitor
	settings.Initialize(rootDir, true)
	if got, want := goolib.InactivityMode, goolib.InactivityMonitor; got != want {
		t.Errorf("goolib.InactivityMode with inactivitymode: kill = %q, want %q", got, want)
	}
}

func TestProgressDefault(t *testing.T) {
	rootDir := t.TempDir()
	conf := filepath.Join(rootDir, "googet.conf")
	if err := os.WriteFile(conf, []byte("progress: false"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	settings.Initialize(rootDir, true)
	if settings.Progress {
		t.Fatalf("settings.Progress with progress: false = true, want false")
	}

	// An absent key restores the default rather than keeping the prior value.
	if err := os.WriteFile(conf, []byte("cachelife: 10m"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	settings.Initialize(rootDir, true)
	if !settings.Progress {
		t.Errorf("settings.Progress with no progress key = false, want true")
	}
}
