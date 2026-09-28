package settings_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/supervisor"
)

func TestInitialize(t *testing.T) {
	rootDir := t.TempDir()
	f, err := os.Create(filepath.Join(rootDir, "googet.conf"))
	if err != nil {
		t.Fatalf("error creating conf file: %v", err)
	}
	content := []byte("archs: [noarch, x86_64, arm64]\ncachelife: 10m\nlockfilemaxage: 1x\nallowunsafeurl: true")
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
}

func TestInitializeSupervisorSettings(t *testing.T) {
	tests := []struct {
		name                        string
		content                     string
		wantMode                    supervisor.Mode
		wantInactivity, wantInstall time.Duration
		wantUIGrace, wantStall      time.Duration
		wantUIDetection             bool
	}{
		{
			name:            "defaults when unset",
			content:         "archs: [noarch]",
			wantUIDetection: true,
		},
		{
			name:            "explicit values",
			content:         "archs: [noarch]\nsupervisormode: monitor\ninactivitytimeout: 10m\ninstalltimeout: 3h\nuigraceperiod: 45s\ndownloadstalltimeout: 5m\nuidetection: false",
			wantMode:        supervisor.ModeMonitor,
			wantInactivity:  10 * time.Minute,
			wantInstall:     3 * time.Hour,
			wantUIGrace:     45 * time.Second,
			wantStall:       5 * time.Minute,
			wantUIDetection: false,
		},
		{
			name:            "mode off",
			content:         "archs: [noarch]\nsupervisormode: OFF",
			wantMode:        supervisor.ModeOff,
			wantUIDetection: true,
		},
		{
			name:            "zero disables timeouts",
			content:         "archs: [noarch]\ninactivitytimeout: 0\ninstalltimeout: 0s",
			wantInactivity:  -1,
			wantInstall:     -1,
			wantUIDetection: true,
		},
		{
			name:            "zero does not disable stall or ui grace",
			content:         "archs: [noarch]\ndownloadstalltimeout: 0\nuigraceperiod: 0s",
			wantUIDetection: true,
		},
		{
			name:            "invalid values fall back to defaults",
			content:         "archs: [noarch]\nsupervisormode: aggressive\ninactivitytimeout: soon\ninstalltimeout: -1h\nuigraceperiod: 0\ndownloadstalltimeout: -5s",
			wantUIDetection: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rootDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(tc.content), 0644); err != nil {
				t.Fatalf("os.WriteFile: %v", err)
			}
			settings.Initialize(rootDir, false)
			if settings.SupervisorMode != tc.wantMode {
				t.Errorf("SupervisorMode = %v, want %v", settings.SupervisorMode, tc.wantMode)
			}
			if settings.InactivityTimeout != tc.wantInactivity {
				t.Errorf("InactivityTimeout = %v, want %v", settings.InactivityTimeout, tc.wantInactivity)
			}
			if settings.InstallTimeout != tc.wantInstall {
				t.Errorf("InstallTimeout = %v, want %v", settings.InstallTimeout, tc.wantInstall)
			}
			if settings.UIGracePeriod != tc.wantUIGrace {
				t.Errorf("UIGracePeriod = %v, want %v", settings.UIGracePeriod, tc.wantUIGrace)
			}
			if settings.DownloadStallTimeout != tc.wantStall {
				t.Errorf("DownloadStallTimeout = %v, want %v", settings.DownloadStallTimeout, tc.wantStall)
			}
			if settings.UIDetection != tc.wantUIDetection {
				t.Errorf("UIDetection = %v, want %v", settings.UIDetection, tc.wantUIDetection)
			}
		})
	}
}
