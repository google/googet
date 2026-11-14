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

package system

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

func TestSpecUninstall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ps     goolib.PkgSpec
		want   goolib.ExecFile
		wantOK bool
	}{
		{
			name:   "uninstall script",
			ps:     goolib.PkgSpec{Uninstall: goolib.ExecFile{Path: "u.ps1", Args: []string{"x"}, ExitCodes: []int{3}}},
			want:   goolib.ExecFile{Path: "u.ps1", Args: []string{"x"}, ExitCodes: []int{3}},
			wantOK: true,
		},
		{
			name: "msix keeps uninstall args and exit codes",
			ps: goolib.PkgSpec{
				Install:   goolib.ExecFile{Path: "app.msix", Args: []string{"install-arg"}},
				Uninstall: goolib.ExecFile{Args: []string{"a"}, ExitCodes: []int{5}},
			},
			want:   goolib.ExecFile{Path: "app.msix", Args: []string{"a"}, ExitCodes: []int{5}},
			wantOK: true,
		},
		{
			name:   "msixbundle",
			ps:     goolib.PkgSpec{Install: goolib.ExecFile{Path: `sub\app.msixbundle`}},
			want:   goolib.ExecFile{Path: `sub\app.msixbundle`},
			wantOK: true,
		},
		{
			name: "uninstall script wins over msix",
			ps: goolib.PkgSpec{
				Install:   goolib.ExecFile{Path: "app.msix"},
				Uninstall: goolib.ExecFile{Path: "u.ps1", Args: []string{"y"}},
			},
			want:   goolib.ExecFile{Path: "u.ps1", Args: []string{"y"}},
			wantOK: true,
		},
		{
			name:   "msi from registry",
			ps:     goolib.PkgSpec{Install: goolib.ExecFile{Path: "setup.msi"}},
			wantOK: false,
		},
		{
			name:   "exe from registry",
			ps:     goolib.PkgSpec{Install: goolib.ExecFile{Path: "setup.exe"}},
			wantOK: false,
		},
		{
			// The extension comparison is case sensitive, as it was before
			// specUninstall.
			name:   "upper case MSIX",
			ps:     goolib.PkgSpec{Install: goolib.ExecFile{Path: "app.MSIX"}},
			wantOK: false,
		},
		{
			name:   "files only",
			ps:     goolib.PkgSpec{},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := specUninstall(&tc.ps)
			if ok != tc.wantOK {
				t.Errorf("specUninstall() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok {
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("specUninstall() mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestRemoveUninstallEntryMissing(t *testing.T) {
	name := fmt.Sprintf("googet-test-missing-%d", time.Now().UnixNano())
	if err := RemoveUninstallEntry(name); err != nil {
		t.Errorf("RemoveUninstallEntry(%q) = %v, want nil for a missing entry", name, err)
	}
}
