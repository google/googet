//go:build linux || darwin
// +build linux darwin

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
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
)

// RemoveOnReboot not implemented on non Windows.
func RemoveOnReboot(name string) error {
	return nil
}

// removeLater reports that name couldn't be removed now. Removal on reboot
// isn't available on this system, so it always returns an error.
func removeLater(name string) error {
	return fmt.Errorf("removal of %s on reboot is not supported on this system", name)
}

// IsTransientShareError reports whether err is a Windows sharing error that
// a reader may see while an elevated writer replaces or removes a file. It
// always returns false on this system.
func IsTransientShareError(err error) bool {
	return false
}

// Open calls os.Open
func Open(name string) (*os.File, error) {
	return os.Open(name)
}

// Create calls os.Create
func Create(name string) (*os.File, error) {
	return os.Create(name)
}

// OpenFile calls os.OpenFile
func OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

// OpenShared calls os.Open. On Windows it also shares delete access, so an
// open reader never stops an elevated writer from renaming or deleting the
// file; Unix places no such restriction on renames or deletes.
//
// Callers must read what they need and close the file straight away, and
// must never hold the handle across network or other long operations.
func OpenShared(name string) (*os.File, error) {
	return os.Open(name)
}

// Remove calls os.Remove
func Remove(name string) error {
	return os.Remove(name)
}

// RemoveAll calls os.RemoveAll
func RemoveAll(name string) error {
	return os.RemoveAll(name)
}

// Mkdir calls os.Mkdir
func Mkdir(name string, mode os.FileMode) error {
	return os.Mkdir(name, mode)
}

// MkdirAll calls os.MkdirAll
func MkdirAll(name string, mode os.FileMode) error {
	return os.MkdirAll(name, mode)
}

// Rename calls os.Rename
func Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// Lstat calls os.Lstat
func Lstat(name string) (os.FileInfo, error) {
	return os.Lstat(name)
}

// Stat calls os.Stat
func Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

// ReadDir calls ioutil.ReadDir
// Upgrade to os.ReadDir in future. https://golang.org/pkg/io/ioutil/#ReadDir
func ReadDir(root string) ([]os.FileInfo, error) {
	return ioutil.ReadDir(root)
}

// Walk calls filepath.Walk
func Walk(root string, walkFn filepath.WalkFunc) error {
	return filepath.Walk(root, walkFn)
}
