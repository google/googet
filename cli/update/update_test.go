package update

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
)

func captureStdout(f func()) string {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestUpdates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pm       client.PackageMap
		rm       client.RepoMap
		dryRun   bool
		want     []goolib.PackageInfo
		wantStrs []string
	}{
		{
			name: "upgrade to later version",
			pm: client.PackageMap{
				"foo.x86_32": "1.0",
				"bar.x86_32": "2.0",
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
			},
			want: []goolib.PackageInfo{{Name: "foo", Arch: "x86_32", Ver: "2.0"}},
		},
		{
			name: "rollback to earlier version",
			pm: client.PackageMap{
				"foo.x86_32": "2.0",
				"bar.x86_32": "2.0",
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
				"rollback": client.Repo{
					Priority: 1500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			want: []goolib.PackageInfo{{Name: "foo", Arch: "x86_32", Ver: "1.0"}},
		},
		{
			name: "no change if rollback version already installed",
			pm: client.PackageMap{
				"foo.x86_32": "1.0",
			},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
						{PackageSpec: &goolib.PkgSpec{Name: "bar", Version: "2.0", Arch: "x86_32"}},
					},
				},
				"rollback": client.Repo{
					Priority: 1500,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			want: nil,
		},
		{
			name: "dry run with updates",
			pm:   client.PackageMap{"foo.x86_32": "1.0"},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "2.0", Arch: "x86_32"}},
					},
				},
			},
			dryRun: true,
			want:   []goolib.PackageInfo{{Name: "foo", Arch: "x86_32", Ver: "2.0"}},
			wantStrs: []string{
				"Dry run: Searching for available updates...",
				"  foo.x86_32, 1.0 --> 2.0 from stable",
			},
		},
		{
			name: "dry run no updates",
			pm:   client.PackageMap{"foo.x86_32": "1.0"},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			dryRun: true,
			want:   nil,
			wantStrs: []string{
				"Dry run: Searching for available updates...",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			settings.Archs = []string{"x86_32", "noarch"}

			var pi []goolib.PackageInfo
			output := captureStdout(func() {
				pi = updates(tc.pm, tc.rm, tc.dryRun)
			})

			if diff := cmp.Diff(pi, tc.want); diff != "" {
				t.Errorf("updates(%v, %v, %v) got unexpected diff (-got +want):\n%v", tc.pm, tc.rm, tc.dryRun, diff)
			}

			for _, want := range tc.wantStrs {
				if !strings.Contains(output, want) {
					t.Errorf("Expected stdout to contain %q, got:\n%s", want, output)
				}
			}
		})
	}
}
