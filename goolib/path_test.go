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

package goolib

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type joinWithinCase struct {
	name    string
	want    string
	wantErr bool
}

func checkJoinWithin(t *testing.T, dir string, cases []joinWithinCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JoinWithin(dir, tc.name)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("JoinWithin(%q, %q) = %q, want error", dir, tc.name, got)
				}
				if !strings.Contains(err.Error(), "path traversal") {
					t.Errorf("JoinWithin(%q, %q) error = %v, want a path traversal error", dir, tc.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("JoinWithin(%q, %q) error: %v", dir, tc.name, err)
			}
			if got != tc.want {
				t.Errorf("JoinWithin(%q, %q) = %q, want %q", dir, tc.name, got, tc.want)
			}
		})
	}
}

func TestJoinWithin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	checkJoinWithin(t, dir, []joinWithinCase{
		{name: "file", want: filepath.Join(dir, "file")},
		{name: "a/b/file", want: filepath.Join(dir, "a", "b", "file")},
		{name: "a/../file", want: filepath.Join(dir, "file")},
		{name: ".", want: dir},
		{name: "./", want: dir},
		{name: "..pkg", want: filepath.Join(dir, "..pkg")},
		// Absolute names are resolved under dir, as filepath.Join does.
		{name: "/file", want: filepath.Join(dir, "file")},
		// Leaving and re-entering dir lexically lands back inside it.
		{name: "../pkg/file", want: filepath.Join(dir, "file")},
		{name: "..", wantErr: true},
		{name: "../file", wantErr: true},
		{name: "a/../../file", wantErr: true},
		{name: "../../pkg/file", wantErr: true},
		// A sibling directory sharing dir's name as a prefix is outside dir.
		{name: "../pkg-evil/file", wantErr: true},
	})
}

func TestJoinWithinWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("backslash is a path separator only on Windows")
	}
	dir := filepath.Join(t.TempDir(), "pkg")
	checkJoinWithin(t, dir, []joinWithinCase{
		{name: `a\file`, want: filepath.Join(dir, "a", "file")},
		{name: `..\file`, wantErr: true},
		{name: `a\..\..\file`, wantErr: true},
		{name: `..\pkg-evil\file`, wantErr: true},
		// Volume and UNC prefixes in a later element are not special to
		// filepath.Join, so they stay under dir as ordinary path elements.
		{name: `C:\file`, want: dir + `\C:\file`},
		{name: `C:foo`, want: dir + `\C:foo`},
		{name: `\\server\share\file`, want: dir + `\server\share\file`},
	})
}

func TestJoinWithinRelativeDir(t *testing.T) {
	checkJoinWithin(t, "pkg", []joinWithinCase{
		{name: "a/file", want: filepath.Join("pkg", "a", "file")},
		{name: "../pkg2/file", wantErr: true},
	})
}

func TestJoinWithinEmptyDir(t *testing.T) {
	if got, err := JoinWithin("", "/x"); err == nil {
		t.Errorf("JoinWithin(%q, %q) = %q, want error", "", "/x", got)
	}
}
