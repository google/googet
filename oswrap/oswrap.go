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
	"io"
	"os"
	"path/filepath"
	"time"
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
