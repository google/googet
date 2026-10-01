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

package install

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

// backupInfix is inserted between a file name and a random suffix to form the
// name of the backup made before the file is overwritten.
const backupInfix = ".googet-bak-"

// dirModeMask selects the directory mode bits that rollback restores when it
// recreates a directory.
const dirModeMask = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// installOps holds the filesystem and installer operations performed while
// installing a package. Production code uses defaultInstallOps; tests build
// their own value to simulate failures without mutating package state.
type installOps struct {
	// backup renames an existing file to a unique backup path in the same
	// directory and returns the backup path.
	backup func(path string) (string, error)
	// copyBackup copies an existing file to a unique backup path in the same
	// directory and returns the backup path. It is used when backup fails.
	copyBackup func(path string) (string, error)
	// removeOrRename deletes a file when possible and otherwise renames it. It
	// returns the new name, or an empty string if the file was deleted.
	removeOrRename func(path string) (string, error)
	// copyContents copies package file contents to their destination.
	copyContents func(dst io.Writer, src io.Reader) (int64, error)
	// systemInstall runs the package's system specific installer.
	systemInstall func(dir string, ps *goolib.PkgSpec) error
}

// defaultInstallOps returns the operations used by production installs.
func defaultInstallOps() installOps {
	return installOps{
		backup:         renameToBackup,
		copyBackup:     copyToBackup,
		removeOrRename: client.RemoveOrRename,
		copyContents:   io.Copy,
		systemInstall:  system.Install,
	}
}

// movedFile tracks a file that was moved or copied to a temporary backup.
type movedFile struct {
	originalPath string
	backupPath   string
}

// removedDir tracks a pre-existing empty directory that was removed to make
// room for a file.
type removedDir struct {
	path string
	mode os.FileMode
}

// installTxn places the files of a single package and records every change it
// makes to the filesystem so that the changes can be undone if the install
// fails.
type installTxn struct {
	// ops performs the filesystem and installer operations.
	ops installOps
	// dbOnly records files in insFiles without touching the filesystem.
	dbOnly bool
	// force overwrites files owned by other packages even with StrictConflicts.
	force bool
	// conflictMap maps files owned by other packages to their owner.
	conflictMap map[string]string
	// insFiles maps each placed path to its checksum, or to an empty string for
	// directories.
	insFiles map[string]string

	// created holds files that did not exist before the install wrote them.
	created map[string]bool
	// createdDirs holds directories that did not exist before the install.
	createdDirs []string
	// removedDirs holds empty directories removed to make room for files.
	removedDirs []removedDir
	// moved holds pre-existing files that were moved to backups, in order.
	moved []movedFile
	// backedUp holds original paths that already have a backup in moved.
	backedUp map[string]bool
}

// newInstallTxn returns an empty transaction that uses ops and records placed
// files in a fresh insFiles map.
func newInstallTxn(ops installOps, dbOnly, force bool, conflictMap map[string]string) *installTxn {
	return &installTxn{
		ops:         ops,
		dbOnly:      dbOnly,
		force:       force,
		conflictMap: conflictMap,
		insFiles:    make(map[string]string),
		created:     make(map[string]bool),
		backedUp:    make(map[string]bool),
	}
}

// newBackupPath returns a randomly named sibling of path that uses the backup
// naming scheme.
func newBackupPath(path string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return path + backupInfix + hex.EncodeToString(b), nil
}

// renameToBackup renames path to a unique, randomly named sibling and returns
// the new name. Renaming within the same directory avoids cross-volume moves
// and works on Windows even for executables that are currently running.
func renameToBackup(path string) (string, error) {
	var lastErr error
	for i := 0; i < 10; i++ {
		bak, err := newBackupPath(path)
		if err != nil {
			return "", err
		}
		if _, err := oswrap.Lstat(bak); err == nil {
			lastErr = fmt.Errorf("backup path %q already exists", bak)
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := oswrap.Rename(path, bak); err != nil {
			return "", err
		}
		return bak, nil
	}
	return "", lastErr
}

// copyToBackup copies path to a unique, randomly named sibling with the same
// permissions and returns the new name. It leaves path in place and removes a
// partial copy on failure.
func copyToBackup(path string) (string, error) {
	src, err := oswrap.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return "", err
	}
	perm := fi.Mode().Perm()

	var bak string
	var dst *os.File
	for i := 0; i < 10; i++ {
		if bak, err = newBackupPath(path); err != nil {
			return "", err
		}
		// The owner write bit is added so the copy can be written; the exact
		// permissions are applied once the copy is complete.
		dst, err = oswrap.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm|0200)
		if err == nil || !os.IsExist(err) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// oswrap has no Chmod wrapper, so os.Chmod is used directly.
		err = os.Chmod(bak, perm)
	}
	if err != nil {
		if rerr := oswrap.Remove(bak); rerr != nil && !os.IsNotExist(rerr) {
			logger.Errorf("Failed to remove partial backup copy %q: %v.", bak, rerr)
		}
		return "", err
	}
	return bak, nil
}

// mkdirAllTracked behaves like oswrap.MkdirAll but records every directory it
// creates so that rollback can remove them.
func (txn *installTxn) mkdirAllTracked(path string, mode os.FileMode) error {
	var missing []string
	for p := filepath.Clean(path); ; {
		if _, err := oswrap.Lstat(p); !os.IsNotExist(err) {
			break
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if err := oswrap.MkdirAll(path, mode); err != nil {
		return err
	}
	// Record shallowest first; rollback removes them deepest first.
	for i := len(missing) - 1; i >= 0; i-- {
		txn.createdDirs = append(txn.createdDirs, missing[i])
	}
	return nil
}

// recordMoved records that the content of originalPath now lives at
// backupPath.
func (txn *installTxn) recordMoved(originalPath, backupPath string) {
	txn.moved = append(txn.moved, movedFile{originalPath: originalPath, backupPath: backupPath})
	txn.backedUp[originalPath] = true
}

// discardBackup removes a backup that is no longer needed. An empty path is
// ignored.
func (txn *installTxn) discardBackup(path string) {
	if path == "" {
		return
	}
	if err := oswrap.Remove(path); err != nil && !os.IsNotExist(err) {
		logger.Errorf("Failed to remove redundant backup %q: %v.", path, err)
	}
}

// prepareTarget makes outPath ready to be written. An existing file is moved
// to a backup so it can be restored on rollback; a missing file is recorded as
// created before it is written so that a partial copy is also rolled back.
func (txn *installTxn) prepareTarget(outPath string) error {
	if txn.created[outPath] || txn.backedUp[outPath] {
		// This install already owns the current content of outPath.
		return nil
	}
	fi, err := oswrap.Lstat(outPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		txn.created[outPath] = true
		return nil
	}
	if fi.IsDir() {
		return txn.removeEmptyDir(outPath, fi.Mode())
	}
	bak, err := txn.ops.backup(outPath)
	if err == nil {
		txn.recordMoved(outPath, bak)
		return nil
	}
	logger.Warningf("Unable to back up %q before overwriting it: %v; falling back to remove or rename.", outPath, err)
	return txn.fallbackBackup(outPath)
}

// removeEmptyDir removes the empty directory at path so that a file can take
// its place, and records it so that rollback recreates it with mode. This
// preserves the legacy behavior; a non-empty directory causes an error.
func (txn *installTxn) removeEmptyDir(path string, mode os.FileMode) error {
	fn, err := txn.ops.removeOrRename(path)
	if err != nil {
		return err
	}
	if fn != "" {
		txn.recordMoved(path, fn)
		return nil
	}
	txn.removedDirs = append(txn.removedDirs, removedDir{path: path, mode: mode})
	txn.created[path] = true
	return nil
}

// fallbackBackup clears outPath after the same-directory rename to a backup
// failed. The file is first copied to a sibling backup because removeOrRename
// may delete it; if removeOrRename instead preserves the original under a new
// name, that name is used as the backup and the copy is discarded.
func (txn *installTxn) fallbackBackup(outPath string) error {
	cp, cpErr := txn.ops.copyBackup(outPath)
	if cpErr != nil {
		logger.Warningf("Unable to copy %q to a backup: %v.", outPath, cpErr)
	}
	fn, err := txn.ops.removeOrRename(outPath)
	if err != nil {
		// The original file is still in place, so the copy is not needed.
		txn.discardBackup(cp)
		return err
	}
	switch {
	case fn != "":
		txn.discardBackup(cp)
		txn.recordMoved(outPath, fn)
	case cpErr == nil:
		txn.recordMoved(outPath, cp)
	default:
		logger.Warningf("Existing file %q was deleted before being overwritten; it cannot be restored if the install fails.", outPath)
		txn.created[outPath] = true
	}
	return nil
}

// rollback undoes the recorded changes: files the install created are
// removed, backups are restored in reverse order, removed empty directories are
// recreated, and directories the install created are removed deepest first if
// they are empty.
func (txn *installTxn) rollback() {
	for file := range txn.created {
		if err := oswrap.Remove(file); err != nil && !os.IsNotExist(err) {
			logger.Errorf("Failed to remove newly placed file %q during rollback: %v.", file, err)
		}
	}
	for i := len(txn.moved) - 1; i >= 0; i-- {
		mf := txn.moved[i]
		if _, err := oswrap.Lstat(mf.originalPath); err == nil {
			if err := oswrap.Remove(mf.originalPath); err != nil && !os.IsNotExist(err) {
				logger.Errorf("Failed to remove file %q during rollback: %v.", mf.originalPath, err)
			}
		}
		if err := oswrap.Rename(mf.backupPath, mf.originalPath); err != nil {
			logger.Errorf("Failed to restore backup %q to %q during rollback: %v.", mf.backupPath, mf.originalPath, err)
		}
	}
	for i := len(txn.removedDirs) - 1; i >= 0; i-- {
		rd := txn.removedDirs[i]
		if err := oswrap.Mkdir(rd.path, rd.mode&dirModeMask); err != nil {
			logger.Errorf("Failed to recreate directory %q during rollback: %v.", rd.path, err)
			continue
		}
		// Mkdir is subject to the umask, so apply the original mode explicitly.
		// oswrap has no Chmod wrapper, so os.Chmod is used directly.
		if err := os.Chmod(rd.path, rd.mode&dirModeMask); err != nil {
			logger.Errorf("Failed to restore mode of directory %q during rollback: %v.", rd.path, err)
		}
	}
	for i := len(txn.createdDirs) - 1; i >= 0; i-- {
		d := txn.createdDirs[i]
		// Remove fails on non-empty directories, which must be left in place.
		if err := oswrap.Remove(d); err != nil && !os.IsNotExist(err) {
			logger.Infof("Leaving directory %q during rollback: %v.", d, err)
		}
	}
}

// commit deletes the recorded backups after a successful install. Backups that
// cannot be deleted, e.g. because they are locked, are scheduled for removal
// on reboot.
func (txn *installTxn) commit() {
	for _, mf := range txn.moved {
		err := oswrap.Remove(mf.backupPath)
		if err == nil || os.IsNotExist(err) {
			continue
		}
		logger.Errorf("Failed to remove backup file %q: %v.", mf.backupPath, err)
		if err := oswrap.RemoveOnReboot(mf.backupPath); err != nil {
			logger.Errorf("Failed to schedule removal of backup file %q on reboot: %v.", mf.backupPath, err)
		}
	}
}
