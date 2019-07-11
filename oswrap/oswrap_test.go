/*
Copyright 2016 Google Inc. All Rights Reserved.
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

package oswrap

import (
	"runtime"
	"testing"
)

func TestRootDir(t *testing.T) {
	var table = []struct {
		path, want string
	}{
		{"/linux/abs/path", "/linux"},
		{"linux/rel/path", "linux"},
		{"/path", "/path"},
	}

	if runtime.GOOS == "windows" {
		table = append(table, []struct{ path, want string }{
			{`Z:\windows\abs\path`, `Z:\windows`},
			{`\windows\rel\path`, `\windows`},
		}...)
	}

	for _, tt := range table {
		if got := rootDir(tt.path); got != tt.want {
			t.Fatalf("rootDir did not return expected path, got: %q, want: %q ", got, tt.want)
		}
	}
}
