//+build windows

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
	"os"
	"path/filepath"
	"strings"
)

// normPath transforms a windows path into an extended-length path as described in
// https://msdn.microsoft.com/en-us/library/windows/desktop/aa365247(v=vs.85).aspx#maxpath
func normPath(path string) (string, error) {
	if strings.HasPrefix(path, "\\\\?\\") {
		return path, nil
	}

	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	return "\\\\?\\" + path, nil
}

// Open calls os.Open with name normalized
func Open(name string) (*os.File, error) {
	name, err := normPath(name)
	if err != nil {
		return nil, err
	}
	return os.Open(name)
}

// Create calls os.Create with name normalized
func Create(name string) (*os.File, error) {
	name, err := normPath(name)
	if err != nil {
		return nil, err
	}
	return os.Create(name)
}

// OpenFile calls os.OpenFile with name normalized
func OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	name, err := normPath(name)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(name, flag, perm)
}

// Remove calls os.Remove with name normalized
func Remove(name string) error {
	name, err := normPath(name)
	if err != nil {
		return nil
	}
	return os.Remove(name)
}

// RemoveAll calls os.RemoveAll with name normalized
func RemoveAll(name string) error {
	name, err := normPath(name)
	if err != nil {
		return nil
	}
	return os.RemoveAll(name)
}

// Mkdir calls os.Mkdir with name normalized
func Mkdir(name string, mode os.FileMode) error {
	name, err := normPath(name)
	if err != nil {
		return err
	}
	return os.Mkdir(name, mode)
}

// MkdirAll calls os.MkdirAll with name normalized
func MkdirAll(name string, mode os.FileMode) error {
	name, err := normPath(name)
	if err != nil {
		return err
	}

	// os.MkdirAll does not work with extended-length paths if
	// nothing in the path exists.
	if err := mkRootDir(name, mode); err != nil {
		return err
	}

	return os.MkdirAll(name, mode)
}

// Rename calls os.Rename with name normalized
func Rename(oldpath, newpath string) error {
	oldpath, err := normPath(oldpath)
	if err != nil {
		return err
	}
	newpath, err = normPath(newpath)
	if err != nil {
		return err
	}
	return os.Rename(oldpath, newpath)
}

// Lstat calls os.Lstat with name normalized
func Lstat(name string) (os.FileInfo, error) {
	name, err := normPath(name)
	if err != nil {
		return nil, err
	}
	return os.Lstat(name)
}

// Stat calls os.Stat with name normalized
func Stat(name string) (os.FileInfo, error) {
	name, err := normPath(name)
	if err != nil {
		return nil, err
	}
	return os.Stat(name)
}

// Walk calls filepath.Walk with name normalized, and un-normalizes name before
// calling walkFn
func Walk(root string, walkFn filepath.WalkFunc) error {
	newroot, err := normPath(root)
	if err != nil {
		return err
	}
	return filepath.Walk(newroot, func(path string, info os.FileInfo, err error) error {
		oldpath := root + strings.TrimPrefix(path, newroot)
		return walkFn(oldpath, info, err)
	})
}
