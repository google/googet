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
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// JoinWithin joins name to dir like filepath.Join, and returns an error if the
// cleaned result is outside dir. It compares path components rather than
// string prefixes, so a sibling such as dir+"-evil" counts as outside. As with
// filepath.Join, an absolute name is resolved under dir rather than rejected,
// and the result is relative if dir is relative. The check is lexical and does
// not follow symlinks.
func JoinWithin(dir, name string) (string, error) {
	if dir == "" {
		return "", errors.New("empty directory")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absDir, filepath.Join(absDir, name))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal: %q is outside %q", name, dir)
	}
	return filepath.Join(dir, name), nil
}
