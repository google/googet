package settings_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/settings"
)

func TestInitialize(t *testing.T) {
	rootDir := t.TempDir()
	f, err := os.Create(filepath.Join(rootDir, "googet.conf"))
	if err != nil {
		t.Fatalf("error creating conf file: %v", err)
	}
	content := []byte("archs: [noarch, x86_64, arm64]\ncachelife: 10m\nlockfilemaxage: 1x\nallowunsafeurl: true\nprogress: false")
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

func TestStallTimeout(t *testing.T) {
	oldStall := client.StallTimeout
	oldRoot, oldConfirm, oldArchs := settings.RootDir, settings.Confirm, settings.Archs
	oldCacheLife, oldUnsafe, oldStrict, oldProgress := settings.CacheLife, settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress
	t.Cleanup(func() {
		client.StallTimeout = oldStall
		settings.RootDir, settings.Confirm, settings.Archs = oldRoot, oldConfirm, oldArchs
		settings.CacheLife, settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress = oldCacheLife, oldUnsafe, oldStrict, oldProgress
	})
	rootDir := t.TempDir()
	// Each case starts from the previous one's value, so a bad value must
	// restore the default rather than keep the prior setting.
	for _, tc := range []struct {
		conf string
		want time.Duration
	}{
		{"stalltimeout: 5m", 5 * time.Minute},
		{"stalltimeout: 5x", client.DefaultStallTimeout},
		{"stalltimeout: 30s", 30 * time.Second},
		{"stalltimeout: -1s", client.DefaultStallTimeout},
		{"stalltimeout: 0s", client.DefaultStallTimeout},
		{"cachelife: 10m", client.DefaultStallTimeout},
	} {
		if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(tc.conf), 0644); err != nil {
			t.Fatalf("error writing conf file: %v", err)
		}
		settings.Initialize(rootDir, true)
		if got := client.StallTimeout; got != tc.want {
			t.Errorf("client.StallTimeout with %q = %v, want %v", tc.conf, got, tc.want)
		}
	}
}

func TestSetRootAndLoadConf(t *testing.T) {
	oldStall := client.StallTimeout
	oldRoot, oldConfirm, oldArchs := settings.RootDir, settings.Confirm, settings.Archs
	oldCacheLife, oldLockAge, oldProxy := settings.CacheLife, settings.LockFileMaxAge, settings.ProxyServer
	oldUnsafe, oldStrict, oldProgress := settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress
	t.Cleanup(func() {
		client.StallTimeout = oldStall
		settings.RootDir, settings.Confirm, settings.Archs = oldRoot, oldConfirm, oldArchs
		settings.CacheLife, settings.LockFileMaxAge, settings.ProxyServer = oldCacheLife, oldLockAge, oldProxy
		settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress = oldUnsafe, oldStrict, oldProgress
	})

	settings.CacheLife = 3 * time.Minute
	settings.LockFileMaxAge = 24 * time.Hour
	settings.ProxyServer = ""
	settings.AllowUnsafeURL = false
	settings.StrictConflicts = false
	settings.Progress = true

	rootDir := t.TempDir()
	conf := "cachelife: 7m\nlockfilemaxage: 5m\nproxyserver: http://proxy.example:8080\nallowunsafeurl: true\nstrictconflicts: true\nprogress: false\n"
	if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(conf), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}

	// SetRoot must set paths and Confirm without reading googet.conf.
	settings.SetRoot(rootDir, false)
	if settings.RootDir != rootDir || settings.Confirm {
		t.Fatalf("after SetRoot: RootDir=%q Confirm=%v, want %q false", settings.RootDir, settings.Confirm, rootDir)
	}
	for name, got := range map[string]string{
		"LockFile":  settings.LockFile(),
		"StateFile": settings.StateFile(),
		"DBFile":    settings.DBFile(),
		"ConfFile":  settings.ConfFile(),
		"LogFile":   settings.LogFile(),
		"CacheDir":  settings.CacheDir(),
		"RepoDir":   settings.RepoDir(),
	} {
		if filepath.Dir(got) != rootDir {
			t.Errorf("%s() = %q, want path under %q", name, got, rootDir)
		}
	}
	if settings.CacheLife != 3*time.Minute || settings.LockFileMaxAge != 24*time.Hour || settings.ProxyServer != "" || settings.AllowUnsafeURL || settings.StrictConflicts || !settings.Progress {
		t.Fatalf("SetRoot read googet.conf unexpectedly: CacheLife=%v LockFileMaxAge=%v ProxyServer=%q AllowUnsafeURL=%v StrictConflicts=%v Progress=%v",
			settings.CacheLife, settings.LockFileMaxAge, settings.ProxyServer, settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress)
	}

	// LoadConf reads googet.conf from the directory set by SetRoot.
	settings.LoadConf()
	if settings.CacheLife != 7*time.Minute {
		t.Errorf("CacheLife = %v, want 7m", settings.CacheLife)
	}
	if settings.LockFileMaxAge != 5*time.Minute {
		t.Errorf("LockFileMaxAge = %v, want 5m", settings.LockFileMaxAge)
	}
	if settings.ProxyServer != "http://proxy.example:8080" {
		t.Errorf("ProxyServer = %q, want http://proxy.example:8080", settings.ProxyServer)
	}
	if !settings.AllowUnsafeURL {
		t.Errorf("AllowUnsafeURL = false, want true")
	}
	if !settings.StrictConflicts {
		t.Errorf("StrictConflicts = false, want true")
	}
	if settings.Progress {
		t.Errorf("Progress = true, want false")
	}
}
