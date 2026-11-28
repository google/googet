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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// ReadFileShared reads the whole file name through OpenShared and closes it
// before returning. Use it for every read that can run without the googet
// lock (for example, by an unelevated user), so the reader never stops an
// elevated writer from renaming or deleting the file.
func ReadFileShared(name string) ([]byte, error) {
	f, err := OpenShared(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// WriteFileAtomic replaces the file name with data, so that a concurrent
// reader sees either the old contents or the new contents, never a partial
// file. It writes data to a uniquely named temporary file in the same
// directory, sets its mode to perm, syncs and closes it, and renames it over
// name; on Windows, os.Rename replaces an existing file through MoveFileEx
// with MOVEFILE_REPLACE_EXISTING. It never truncates name in place. If any
// step fails, the temporary file is removed and name is left as it was.
//
// Under legacy (non-POSIX) Windows rename semantics, replacing a file that a
// reader holds open, even through OpenShared, can fail with a sharing error.
// Readers read the whole file and close it at once, so WriteFileAtomic
// retries the rename while it fails with an error that IsTransientShareError
// reports, up to replaceAttempts times over about 1.8 seconds. Elsewhere
// IsTransientShareError is always false and the rename is tried once.
func WriteFileAtomic(name string, data []byte, perm os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return renameReplacing(tmp, name)
}

const (
	// replaceAttempts is how many times WriteFileAtomic tries to rename its
	// temporary file over a target that a reader holds open.
	replaceAttempts = 10
	// replaceFirstDelay is the wait after the first failed attempt. Each
	// later wait doubles, up to replaceMaxDelay, for about 1.8 seconds in
	// total.
	replaceFirstDelay = 10 * time.Millisecond
	replaceMaxDelay   = 400 * time.Millisecond
)

// renameReplacing renames tmp over name, retrying while the rename fails
// with an error that IsTransientShareError reports.
func renameReplacing(tmp, name string) error {
	return retryWhile(IsTransientShareError, func() error { return Rename(tmp, name) })
}

// retryWhile calls op until it succeeds, fails with an error for which
// transient returns false, or has been called replaceAttempts times, and
// returns op's last error.
func retryWhile(transient func(error) bool, op func() error) error {
	delay := replaceFirstDelay
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || !transient(err) || attempt == replaceAttempts {
			return err
		}
		time.Sleep(delay)
		delay = min(2*delay, replaceMaxDelay)
	}
}

// maxAsideBase is the most bytes of the original base name kept in the
// temporary name used by renameAside, so the temporary name stays well within
// file name length limits.
const maxAsideBase = 64

// renameAside renames name, which must not be a directory, to a unique
// dot-prefixed name in the same directory and returns that name, or "" if
// name no longer exists.
func renameAside(name string) (string, error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	base := filepath.Base(name)
	if len(base) > maxAsideBase {
		base = base[:maxAsideBase]
		// Don't split a multi-byte character.
		for !utf8.ValidString(base) {
			base = base[:len(base)-1]
		}
	}
	tmp := filepath.Join(filepath.Dir(name), "."+base+".del-"+hex.EncodeToString(r[:]))
	if err := Rename(name, tmp); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return tmp, nil
}

// renameAndTryRemove removes name without leaving a delete-pending name
// behind. A file or symlink is renamed aside to a unique dot-prefixed name in
// the same directory and then deleted; the rename frees name at once, even
// while a reader holds the file open through OpenShared. Under legacy
// (non-POSIX) Windows delete semantics, deleting an open file in place would
// leave name "delete pending" until every handle is closed, and creating or
// opening a file at name would fail meanwhile. A directory is removed in
// place with Remove, so it must be empty. A missing name isn't an error.
//
// If the renamed file can't be deleted, renameAndTryRemove returns its name
// and the delete error. Otherwise it returns "" and any error from before the
// delete, in which case name is left as it was.
func renameAndTryRemove(name string) (left string, err error) {
	fi, err := Lstat(name)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		if err := Remove(name); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		return "", nil
	}
	tmp, err := renameAside(name)
	if err != nil || tmp == "" {
		return "", err
	}
	if err := removeAside(tmp); err != nil && !os.IsNotExist(err) {
		return tmp, err
	}
	return "", nil
}

// removeAside deletes the file that renameAndTryRemove renamed aside. Tests
// replace it to make that delete fail.
var removeAside = Remove

// RenameAndRemove removes the file, symlink or empty directory name, so that
// name can be created again straight away even while a reader holds the old
// file open through OpenShared; see renameAndTryRemove. If the renamed file
// can't be deleted (for example, because it is a running executable), it is
// scheduled for removal on reboot. On Windows, RenameAndRemove then returns
// nil, or an error if the scheduling fails. Other systems can't remove a file
// on reboot, so RenameAndRemove returns an error that names the file left
// behind. A missing name isn't an error.
func RenameAndRemove(name string) error {
	left, err := renameAndTryRemove(name)
	if left == "" {
		return err
	}
	return removeLeftLater(left, name, err)
}

// RemoveOrPark removes name like RenameAndRemove, but if the renamed file
// can't be deleted, it also moves the file into parkDir when parkDir is on
// the same volume as name, so that name's directory can still be removed. If
// that move fails, the file stays under its dot-prefixed name next to name.
// It then schedules the file for removal on reboot and returns the file's new
// name, so that the caller can try to delete it again later. As with
// RenameAndRemove, that scheduling is only available on Windows; on other
// systems RemoveOrPark returns the new name together with an error.
func RemoveOrPark(name, parkDir string) (string, error) {
	left, err := renameAndTryRemove(name)
	if left == "" {
		return "", err
	}
	if filepath.VolumeName(parkDir) == filepath.VolumeName(name) {
		if parked, perr := park(left, filepath.Base(name), parkDir); perr == nil {
			left = parked
		}
	}
	return left, removeLeftLater(left, name, err)
}

// removeLeftLater schedules left, which is what remains of name after its
// delete failed with err, for removal on reboot. It returns nil if that
// scheduling succeeds, or an error that names left and includes both err and
// the scheduling error.
//
// It schedules through removeLater rather than the exported RemoveOnReboot
// because the Unix RemoveOnReboot returns nil without doing anything, while
// the Unix removeLater returns an error, so the caller learns that a file was
// left behind.
func removeLeftLater(left, name string, err error) error {
	if rerr := removeLater(left); rerr != nil {
		return fmt.Errorf("removing %s (renamed from %s): %v; %v", left, name, err, rerr)
	}
	return nil
}

// park moves the file name into dir under a new unique name that starts with
// base and returns that name.
func park(name, base, dir string) (string, error) {
	f, err := os.CreateTemp(dir, base+".old")
	if err != nil {
		return "", err
	}
	parked := f.Name()
	f.Close()
	if err := Remove(parked); err != nil {
		return "", err
	}
	if err := Rename(name, parked); err != nil {
		return "", err
	}
	return parked, nil
}

func rootDir(name string) string {
	i := len(filepath.VolumeName(name))
	if filepath.IsAbs(name) {
		i++
	}
	if os.IsPathSeparator(name[i]) {
		i++
	}
	for i < len(name) && !os.IsPathSeparator(name[i]) {
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
