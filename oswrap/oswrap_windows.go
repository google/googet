//+build windows

// Package oswrap exists to translate pathnames into extended-length path names
// behind the scenes, so that googet can install packages with deep directory
// structures
package oswrap

import (
	"path/filepath"
	"os"
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
	// os.MkdirAll does not work with extended-length paths if 
	// nothing in the path exists.
	vol := filepath.VolumeName(name)
	i := len(vol) + 1
	for !os.IsPathSeparator(name[i]) {
		i++
	}
	baseDir := filepath.Join(vol, name[len(vol)+1 : i])
	
	if _, err := Stat(baseDir); err != nil {
        	if err := Mkdir(baseDir, mode); err != nil {
        		return err
        	}
    	}
	
	name, err := normPath(name)
	if err != nil {
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
