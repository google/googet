//+build linux darwin

// Package oswrap on unix only exists for api-compatibility with the windows
// version. All functions are simply thin wrappers to the actual os functions.
package oswrap

import (
	"os"
	"path/filepath"
)

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

// Walk calls filepath.Walk
func Walk(root string, walkFn filepath.WalkFunc) error {
	return filepath.Walk(root, walkFn)
}
