package settings_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
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

	if err := settings.Initialize(rootDir, true); err != nil {
		t.Fatalf("settings.Initialize: %v", err)
	}

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
	if err := settings.Initialize(rootDir, true); err != nil {
		t.Fatalf("settings.Initialize: %v", err)
	}
	if settings.Progress {
		t.Fatalf("settings.Progress with progress: false = true, want false")
	}

	// An absent key restores the default rather than keeping the prior value.
	if err := os.WriteFile(conf, []byte("cachelife: 10m"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	if err := settings.Initialize(rootDir, true); err != nil {
		t.Fatalf("settings.Initialize: %v", err)
	}
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
		if err := settings.Initialize(rootDir, true); err != nil {
			t.Fatalf("settings.Initialize: %v", err)
		}
		if got := client.StallTimeout; got != tc.want {
			t.Errorf("client.StallTimeout with %q = %v, want %v", tc.conf, got, tc.want)
		}
	}
}

func TestInitializeUnreadableConf(t *testing.T) {
	rootDir := t.TempDir()
	// A directory exists but can't be read as a file, like a conf that only
	// administrators may read.
	if err := os.Mkdir(filepath.Join(rootDir, "googet.conf"), 0755); err != nil {
		t.Fatalf("error creating conf directory: %v", err)
	}
	err := settings.Initialize(rootDir, false)
	if err == nil {
		t.Fatal("settings.Initialize with an unreadable googet.conf = nil, want an error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("settings.Initialize with an unreadable googet.conf = %v, want an error other than fs.ErrNotExist", err)
	}
}

func TestInitializeMissingConf(t *testing.T) {
	settings.Archs = []string{"stale"}
	if err := settings.Initialize(t.TempDir(), false); err != nil {
		t.Fatalf("settings.Initialize without googet.conf = %v, want nil", err)
	}
	want, err := system.InstallableArchs()
	if err != nil {
		t.Fatalf("system.InstallableArchs: %v", err)
	}
	if diff := cmp.Diff(want, settings.Archs); diff != "" {
		t.Errorf("settings.Archs without googet.conf diff (-want +got):\n%s", diff)
	}
}

func TestInitializeMalformedConf(t *testing.T) {
	defaultArchs, err := system.InstallableArchs()
	if err != nil {
		t.Fatalf("system.InstallableArchs: %v", err)
	}
	for _, tc := range []struct {
		desc      string
		conf      string
		wantArchs []string
	}{
		{
			desc:      "syntax error uses defaults",
			conf:      "archs: [noarch\n",
			wantArchs: defaultArchs,
		},
		{
			desc:      "type error keeps the fields that decoded",
			conf:      "archs: [arm64]\nprogress: sometimes\n",
			wantArchs: []string{"arm64"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			rootDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(tc.conf), 0644); err != nil {
				t.Fatalf("error writing conf file: %v", err)
			}
			settings.Archs = []string{"stale"}
			if err := settings.Initialize(rootDir, false); err != nil {
				t.Fatalf("settings.Initialize with %q = %v, want nil", tc.conf, err)
			}
			if diff := cmp.Diff(tc.wantArchs, settings.Archs); diff != "" {
				t.Errorf("settings.Archs with %q diff (-want +got):\n%s", tc.conf, diff)
			}
		})
	}
}
