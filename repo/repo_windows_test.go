//go:build windows

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

package repo

import (
	"io/fs"
	"testing"

	"github.com/google/googet/v2/oswrap"
	"golang.org/x/sys/windows"
)

func TestSkipLevelWindows(t *testing.T) {
	for _, tc := range []struct {
		desc string
		err  error
		want skipLog
	}{
		{desc: "access denied", err: windows.ERROR_ACCESS_DENIED, want: skipError},
		{desc: "sharing violation", err: windows.ERROR_SHARING_VIOLATION, want: skipInfo},
		{desc: "delete pending", err: windows.ERROR_DELETE_PENDING, want: skipInfo},
		{desc: "file not found", err: windows.ERROR_FILE_NOT_FOUND, want: skipSilently},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			err := &fs.PathError{Op: "open", Path: "x.repo", Err: tc.err}
			if got := skipLevel(err, oswrap.IsTransientShareError); got != tc.want {
				t.Errorf("skipLevel(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}
