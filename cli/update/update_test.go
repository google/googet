package update

import (
	"bytes"
	"io"
	"os"
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
		name string
		pm   client.PackageMap
		rm   client.RepoMap
		want []goolib.PackageInfo
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
			name: "no updates available",
			pm:   client.PackageMap{"foo.x86_32": "1.0"},
			rm: client.RepoMap{
				"stable": client.Repo{
					Priority: priority.Default,
					Packages: []goolib.RepoSpec{
						{PackageSpec: &goolib.PkgSpec{Name: "foo", Version: "1.0", Arch: "x86_32"}},
					},
				},
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			settings.Archs = []string{"x86_32", "noarch"}

			var pi []goolib.PackageInfo
			captureStdout(func() {
				pi = updates(tc.pm, tc.rm)
			})

			if diff := cmp.Diff(pi, tc.want); diff != "" {
				t.Errorf("updates(%v, %v) got unexpected diff (-got +want):\n%v", tc.pm, tc.rm, diff)
			}
		})
	}
}
