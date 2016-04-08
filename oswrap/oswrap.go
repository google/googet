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

// Package oswrap exists to translate pathnames into extended-length path names
// behind the scenes, so that googet can install packages with deep directory
// structures on Windows.
package oswrap

import (
	"os"
	"path/filepath"
)

func rootDir(name string) string {
	i := len(filepath.VolumeName(name))
	if filepath.IsAbs(name) {
		i++
	}
	for !os.IsPathSeparator(name[i]) {
		i++
	}
	return name[:i]
}

func mkRootDir(name string, mode os.FileMode) error {
	rd := rootDir(name)

	if _, err := Stat(rd); err != nil {
		if err := Mkdir(rd, mode); err != nil {
			return err
		}
	}
	return nil
}
