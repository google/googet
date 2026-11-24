//go:build windows

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

package system

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	shareAll      = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	shareNoDelete = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE

	// inspectAccess only reads metadata, so with shareAll it never
	// conflicts with another program that has the entry open.
	inspectAccess = windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES
	// listAccess lists a directory.
	listAccess = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
	// changeAccess renames or deletes an entry. Write attributes is for
	// clearing the read-only attribute.
	changeAccess = windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.SYNCHRONIZE

	// settableFileAttrs are the attributes that FileBasicInfo can set.
	// Attributes maintained by the file system, such as
	// FILE_ATTRIBUTE_REPARSE_POINT and FILE_ATTRIBUTE_DIRECTORY, are excluded.
	// FileBasicInfo treats 0 as leaving the attributes unchanged, so a caller
	// that clears every settable attribute passes FILE_ATTRIBUTE_NORMAL
	// instead.
	settableFileAttrs = windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM | windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_TEMPORARY | windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED | windows.FILE_ATTRIBUTE_OFFLINE
)

// fileBasicInfo is FILE_BASIC_INFO, padded to 8-byte alignment for windows/386.
type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

// fileDispositionInfo is FILE_DISPOSITION_INFO.
type fileDispositionInfo struct {
	DeleteFile bool
}

// fileDispositionInfoEx is FILE_DISPOSITION_INFO_EX.
type fileDispositionInfoEx struct {
	Flags uint32
}

// fileFullDirInfo is FILE_FULL_DIR_INFO; FileName holds FileNameLength bytes.
type fileFullDirInfo struct {
	NextEntryOffset uint32
	FileIndex       uint32
	CreationTime    int64
	LastAccessTime  int64
	LastWriteTime   int64
	ChangeTime      int64
	EndOfFile       int64
	AllocationSize  int64
	FileAttributes  uint32
	FileNameLength  uint32
	EaSize          uint32
	FileName        [1]uint16
}

// fileRenameInformation is FILE_RENAME_INFORMATION; FileName holds
// FileNameLength bytes.
type fileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

// fileIDInfo is FILE_ID_INFO, holding a 64-bit volume serial number and a
// 128-bit file ID (supported on NTFS and ReFS).
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// fileID identifies a file on a volume, using 128-bit FileIdInfo where
// supported and falling back to 64-bit ByHandleFileInformation otherwise.
type fileID struct {
	volume uint64
	id     [16]byte
	is128  bool
}

func (id fileID) key() string {
	if id.is128 {
		return fmt.Sprintf("id128:%016x:%x", id.volume, id.id)
	}
	return fmt.Sprintf("id64:%016x:%x", id.volume, id.id[:8])
}

// dirEntry is an entry of a directory listing.
type dirEntry struct {
	name  string
	attrs uint32
}

// entryInfo is what inspectEntry reads.
type entryInfo struct {
	state entryState
	id    fileID
}

// now returns the time used in quarantine names. It is a variable so that
// tests can fix it.
var now = time.Now

// CheckEntries checks the direct children of the googet root, and of its
// subdirectories, that rules name, and applies the rules' actions to those
// that aren't trusted. An entry is trusted if it isn't a reparse point, has
// one hard link, its owner is trusted as PrepareRoot defines it, and its
// DACL, explicit and inherited ACEs alike, lets no one else change it. prep
// is the report of PrepareRoot for the same root: an entry that wasn't
// trusted before PrepareRoot reset the permissions above it isn't trusted.
// Links are never followed.
func CheckEntries(root string, rules []EntryRule, prep RootReport) (EntryReport, error) {
	var r EntryReport
	if err := validateRules(rules); err != nil {
		return r, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return r, err
	}
	err = runWithPrivileges(func() error { return checkEntries(root, rules, prep, &r) })
	return r, err
}

func checkEntries(root string, rules []EntryRule, prep RootReport, r *EntryReport) error {
	// PrepareRoot has checked the root and its subdirectories, and only
	// trusted principals can rename them, so they are shared for delete:
	// another googet run that is preparing the root may open them with
	// delete access.
	rh, err := openDir(root, shareAll)
	if err != nil {
		return err
	}
	if rh == windows.InvalidHandle {
		return fmt.Errorf("the googet root %q doesn't exist", root)
	}
	defer windows.CloseHandle(rh)
	for _, dir := range ruleDirs(rules) {
		if err := checkDir(root, dir, rh, rules, prep, r); err != nil {
			return err
		}
	}
	return nil
}

func checkDir(root, dir string, rh windows.Handle, rules []EntryRule, prep RootReport, r *EntryReport) error {
	path, h := root, rh
	if dir != "" {
		path = filepath.Join(root, dir)
		var err error
		if h, err = openDir(path, shareAll); err != nil {
			return err
		}
		if h == windows.InvalidHandle {
			return nil
		}
		defer windows.CloseHandle(h)
	}
	entries, err := listDir(h)
	if err != nil {
		return fmt.Errorf("listing %q: %w", path, err)
	}
	for _, e := range entries {
		rule, ok := matchRule(rules, dir, e.name)
		if !ok {
			continue
		}
		if err := checkEntry(filepath.Join(path, e.name), rule.Action, prep, r); err != nil {
			return err
		}
	}
	return nil
}

func checkEntry(path string, action EntryAction, prep RootReport, r *EntryReport) error {
	info, err := inspectEntry(path)
	if err != nil || info == nil {
		return err
	}
	reasons := combineReasons(entryReasons(info.state, trustedOwner), prep.beforeReasons(info.id.key(), path))
	op, fail := planEntry(action, info.state.IsLink, len(reasons) > 0)
	var newPath string
	switch op {
	case opQuarantine:
		newPath = quarantinePath(path, now())
		if err := quarantine(path, info, filepath.Base(newPath)); err != nil {
			return fmt.Errorf("renaming %q, which %s, to %q: %w; remove it and run googet again", path, strings.Join(reasons, ", "), newPath, err)
		}
		if r.Quarantined == nil {
			r.Quarantined = make(map[string]string)
		}
		r.Quarantined[path] = newPath
	case opRemove:
		if err := remove(path, info); err != nil {
			return fmt.Errorf("removing %q, which %s: %w; remove it and run googet again", path, strings.Join(reasons, ", "), err)
		}
		r.Removed = append(r.Removed, path)
	}
	if fail {
		r.Failures = append(r.Failures, failureMessage(path, reasons, op, newPath, info.state))
	}
	return nil
}

// openDir opens the directory path without following reparse points. It
// returns InvalidHandle and no error if path doesn't exist.
func openDir(path string, share uint32) (windows.Handle, error) {
	h, err := openNoFollow(path, listAccess, share)
	if notFound(err) {
		return windows.InvalidHandle, nil
	}
	if err != nil {
		return windows.InvalidHandle, openError("the googet directory", path, err)
	}
	isDir, isLink, err := fileType(h)
	if err == nil && (isLink || !isDir) {
		err = fmt.Errorf("%q isn't a directory, or is a junction, symbolic link or other reparse point; remove it and run googet again", path)
	}
	if err != nil {
		windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	return h, nil
}

// listDir lists the directory open as h, without "." and "..".
func listDir(h windows.Handle) ([]dirEntry, error) {
	// FILE_FULL_DIR_INFO entries are 8-byte aligned.
	buf := make([]uint64, 8192)
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), len(buf)*8)
	class := uint32(windows.FileFullDirectoryRestartInfo)
	var entries []dirEntry
	for {
		err := windows.GetFileInformationByHandleEx(h, class, &b[0], uint32(len(b)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		class = windows.FileFullDirectoryInfo
		for off := uint32(0); ; {
			info := (*fileFullDirInfo)(unsafe.Pointer(&b[off]))
			name := string(utf16.Decode(unsafe.Slice(&info.FileName[0], info.FileNameLength/2)))
			if name != "." && name != ".." {
				entries = append(entries, dirEntry{name: name, attrs: info.FileAttributes})
			}
			if info.NextEntryOffset == 0 {
				break
			}
			off += info.NextEntryOffset
		}
	}
}

// inspectEntry reads the entry at path itself, even if it is a reparse
// point. It returns nil and no error if path doesn't exist.
func inspectEntry(path string) (*entryInfo, error) {
	h, err := openNoFollow(path, inspectAccess, shareAll)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}
	defer windows.CloseHandle(h)
	isDir, isLink, err := fileType(h)
	if err != nil {
		return nil, fmt.Errorf("reading the attributes of %q: %w", path, err)
	}
	id, links, err := fileIdentity(h)
	if err != nil {
		return nil, fmt.Errorf("reading the attributes of %q: %w", path, err)
	}
	s, err := readState(h)
	if err != nil {
		return nil, fmt.Errorf("reading the permissions of %q: %w", path, err)
	}
	return &entryInfo{state: entryState{ACL: s, IsDir: isDir, IsLink: isLink, Links: links}, id: id}, nil
}

func fileIdentity(h windows.Handle) (fileID, uint32, error) {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fileID{}, 0, err
	}
	var idInfo fileIDInfo
	err := windows.GetFileInformationByHandleEx(h, windows.FileIdInfo, (*byte)(unsafe.Pointer(&idInfo)), uint32(unsafe.Sizeof(idInfo)))
	if err == nil {
		return fileID{volume: idInfo.VolumeSerialNumber, id: idInfo.FileID, is128: true}, fi.NumberOfLinks, nil
	}
	if !errors.Is(err, windows.ERROR_INVALID_PARAMETER) && !errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		return fileID{}, 0, err
	}
	var id fileID
	id.volume = uint64(fi.VolumeSerialNumber)
	binary.LittleEndian.PutUint32(id.id[0:4], fi.FileIndexLow)
	binary.LittleEndian.PutUint32(id.id[4:8], fi.FileIndexHigh)
	return id, fi.NumberOfLinks, nil
}

// openSame opens path without following reparse points and checks that it is
// still the entry (same file ID, directory flag and reparse-point flag) that
// inspectEntry read.
func openSame(path string, info *entryInfo, access, share uint32) (_ windows.Handle, err error) {
	h, err := openNoFollow(path, access, share)
	if err != nil {
		return windows.InvalidHandle, err
	}
	defer func() {
		if err != nil {
			windows.CloseHandle(h)
		}
	}()
	id, _, err := fileIdentity(h)
	if err != nil {
		return windows.InvalidHandle, err
	}
	isDir, isLink, err := fileType(h)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if id != info.id || isDir != info.state.IsDir || isLink != info.state.IsLink {
		return windows.InvalidHandle, errors.New("it changed while googet checked it")
	}
	return h, nil
}

// quarantine renames the entry at path to name in the same directory.
func quarantine(path string, info *entryInfo, name string) error {
	h, err := openSame(path, info, changeAccess, shareAll)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	u, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	u = u[:len(u)-1]
	size := int(unsafe.Offsetof(fileRenameInformation{}.FileName)) + len(u)*2
	buf := make([]uint64, (size+7)/8)
	ri := (*fileRenameInformation)(unsafe.Pointer(&buf[0]))
	// A name without a directory and without RootDirectory renames the
	// entry in its directory. An existing entry with that name isn't
	// replaced.
	ri.FileNameLength = uint32(len(u) * 2)
	copy(unsafe.Slice(&ri.FileName[0], len(u)), u)
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&buf[0])), uint32(size), windows.FileRenameInformation)
}

// remove deletes the entry at path, a file, a link or a directory tree,
// without following links.
func remove(path string, info *entryInfo) error {
	access, share := removeAccess(info.state.IsDir && !info.state.IsLink)
	h, err := openSame(path, info, access, share)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return removeOpen(path, h)
}

// removeAccess returns the access and share mode to delete an entry. A
// directory is also listed, and isn't shared for delete, so that no one can
// rename it, or replace it with a link, while its entries are deleted by
// path.
func removeAccess(isDir bool) (uint32, uint32) {
	if isDir {
		return changeAccess | windows.FILE_LIST_DIRECTORY, shareNoDelete
	}
	return changeAccess, shareAll
}

// removeOpen deletes path, open as h, and if it is a directory and not a
// reparse point, its entries first.
func removeOpen(path string, h windows.Handle) error {
	isDir, isLink, err := fileType(h)
	if err != nil {
		return err
	}
	if isDir && !isLink {
		entries, err := listDir(h)
		if err != nil {
			return fmt.Errorf("listing %q: %w", path, err)
		}
		for _, e := range entries {
			if err := removePath(filepath.Join(path, e.name), e.attrs); err != nil {
				return err
			}
		}
	}
	return deleteOpen(h)
}

func removePath(path string, attrs uint32) error {
	wantDir := attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
	access, share := removeAccess(wantDir)
	h, err := openNoFollow(path, access, share)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return openError("the entry", path, err)
	}
	if !wantDir {
		isDir, isLink, err := fileType(h)
		if err != nil {
			windows.CloseHandle(h)
			return err
		}
		if isDir && !isLink {
			windows.CloseHandle(h)
			access, share = removeAccess(true)
			h, err = openNoFollow(path, access, share)
			if notFound(err) {
				return nil
			}
			if err != nil {
				return openError("the entry", path, err)
			}
		}
	}
	defer windows.CloseHandle(h)
	return removeOpen(path, h)
}

// deleteOpen deletes the file open as h when its handles are closed. The
// handle is the file itself, so a link is deleted and never its target. It
// uses POSIX semantics, which remove the name at once, where the file
// system supports them.
func deleteOpen(h windows.Handle) error {
	ex := fileDispositionInfoEx{Flags: windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&ex)), uint32(unsafe.Sizeof(ex)))
	if !errors.Is(err, windows.ERROR_INVALID_PARAMETER) && !errors.Is(err, windows.ERROR_NOT_SUPPORTED) && !errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return err
	}
	if err := clearReadOnly(h); err != nil {
		return err
	}
	info := fileDispositionInfo{DeleteFile: true}
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}

// clearReadOnly clears the read-only attribute of the file open as h, which
// FileDispositionInfo refuses to delete.
func clearReadOnly(h windows.Handle) error {
	var b fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&b)), uint32(unsafe.Sizeof(b))); err != nil {
		return err
	}
	if b.FileAttributes&windows.FILE_ATTRIBUTE_READONLY == 0 {
		return nil
	}
	// Zero times leave the times unchanged.
	b = fileBasicInfo{FileAttributes: b.FileAttributes & settableFileAttrs &^ windows.FILE_ATTRIBUTE_READONLY}
	if b.FileAttributes == 0 {
		b.FileAttributes = windows.FILE_ATTRIBUTE_NORMAL
	}
	return windows.SetFileInformationByHandle(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&b)), uint32(unsafe.Sizeof(b)))
}

// recordEntries records in r why each entry of the root, open as h, and of
// its subdirectories named by subdirs isn't trusted before PrepareRoot
// changes the root's DACL, which replaces the ACEs they inherit. Subdirectories
// are opened without FILE_SHARE_DELETE and locked on the directory object
// itself (without propagating inheritance) before their entries are recorded.
func recordEntries(root string, h windows.Handle, subdirs []string, recorded map[string]bool, r *RootReport) error {
	if err := recordDir(root, h, recorded, r); err != nil {
		return err
	}
	for _, name := range subdirs {
		if err := recordSubdir(filepath.Join(root, name), recorded, r); err != nil {
			return err
		}
	}
	return nil
}

// recordSubdir locks a root subdirectory on the directory object itself (if it
// needs a reset) and records the pre-repair permissions of its children in r.
// Reparse points are skipped so secureSubdir removes them; a non-directory
// entry fails immediately before repairRoot replaces root inheritance.
func recordSubdir(path string, recorded map[string]bool, r *RootReport) error {
	sh, err := openNoFollow(path, rootAccess, shareNoDelete)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return openError("the googet directory", path, err)
	}
	defer windows.CloseHandle(sh)
	isDir, isLink, err := fileType(sh)
	if err != nil {
		return fmt.Errorf("reading the attributes of %q: %w", path, err)
	}
	if isLink {
		return nil
	}
	if !isDir {
		return fmt.Errorf("%q isn't a directory; remove it and run googet again", path)
	}
	s, err := readState(sh)
	if err != nil {
		return fmt.Errorf("reading the permissions of %q: %w", path, err)
	}
	if subdirNeedsReset(s, trustedOwner) {
		if err := lockDirNoPropagate(sh); err != nil {
			return fmt.Errorf("resetting the permissions of %q: %w; remove it and run googet again", path, err)
		}
	}
	return recordDir(path, sh, recorded, r)
}

// recordDir records in r why each entry of dir, open as h, isn't trusted
// because of its DACL, before PrepareRoot changes the DACL of dir or of a
// directory above it. Entries are keyed by file ID, falling back to a path
// key only when an entry couldn't be inspected.
func recordDir(dir string, h windows.Handle, recorded map[string]bool, r *RootReport) error {
	key := entryKey(dir)
	if recorded[key] {
		return nil
	}
	entries, err := listDir(h)
	if err != nil {
		return fmt.Errorf("listing %q: %w", dir, err)
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.name)
		info, err := inspectEntry(path)
		if err != nil {
			r.recordBeforeKey(entryPathKey(path), []string{fmt.Sprintf("couldn't be checked (%v)", err)})
			continue
		}
		if info == nil {
			continue
		}
		r.recordBeforeKey(info.id.key(), aclReasons(info.state.ACL, info.state.IsDir, trustedOwner(info.state.ACL.OwnerSID), trustedOwner))
	}
	recorded[key] = true
	return nil
}
