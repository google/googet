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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// rootAccess is the access PrepareRoot requests on the root and its
	// subdirectories. Write DAC and write owner are for the repair. List
	// directory is a data access, so the handle's share mode, which omits
	// FILE_SHARE_DELETE, is enforced: while the handle is open, nobody can
	// rename or delete the directory.
	rootAccess = windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER | windows.FILE_READ_ATTRIBUTES | windows.FILE_LIST_DIRECTORY
	// linkDeleteAccess is the access requested when reopening a reparse point
	// at a subdirectory path to remove it.
	linkDeleteAccess = windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES
	// ancestorAccess only reads.
	ancestorAccess = windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES

	// maxPreferredLength is MAX_PREFERRED_LENGTH from lmcons.h.
	maxPreferredLength = 0xffffffff
)

var (
	netapi32                    = windows.NewLazySystemDLL("netapi32.dll")
	procNetLocalGroupGetMembers = netapi32.NewProc("NetLocalGroupGetMembers")
)

// writeSecurity, volumeFlags and runWithPrivileges are variables so that tests
// can inject hooks and failures.
var (
	writeSecurity = func(h windows.Handle, info windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
		return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil)
	}
	volumeFlags = func(path string) (string, uint32, error) {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return "", 0, err
		}
		vol := make([]uint16, windows.MAX_LONG_PATH)
		if err := windows.GetVolumePathName(p, &vol[0], uint32(len(vol))); err != nil {
			return "", 0, fmt.Errorf("finding the volume of %q: %w", path, err)
		}
		var flags uint32
		if err := windows.GetVolumeInformation(&vol[0], nil, 0, nil, nil, &flags, nil, 0); err != nil {
			return "", 0, fmt.Errorf("reading the volume information of %q: %w", windows.UTF16ToString(vol), err)
		}
		return windows.UTF16ToString(vol), flags, nil
	}
	runWithPrivileges = withPrivileges
)

// repairPrivileges are enabled while PrepareRoot reads and repairs
// descriptors, so that it can open objects whose DACL doesn't grant
// Administrators access.
var repairPrivileges = [...]string{"SeTakeOwnershipPrivilege", "SeRestorePrivilege", "SeBackupPrivilege"}

// tokenPrivileges is TOKEN_PRIVILEGES with room for repairPrivileges.
type tokenPrivileges struct {
	PrivilegeCount uint32
	Privileges     [len(repairPrivileges)]windows.LUIDAndAttributes
}

// fileAttributeTagInfo is FILE_ATTRIBUTE_TAG_INFO.
type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

func notFound(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

// PrepareRoot creates the googet root with a protected DACL, or checks and
// repairs an existing one, and then checks the subdirectories named by
// subdirs. It refuses a root that is a reparse point or not a directory, a
// root on a volume without persistent ACLs, and a root with a parent that is
// a reparse point. An existing root with an untrusted owner or DACL is
// repaired through a handle that doesn't follow reparse points; a subdirectory
// that is a reparse point is removed, and one with an untrusted owner or ACE
// is reset to inherit from the root. A parent of the root that untrusted
// principals can replace is reported in the warnings.
func PrepareRoot(root string, subdirs []string) (RootReport, error) {
	var r RootReport
	for _, name := range subdirs {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/:`) {
			return r, fmt.Errorf("invalid googet root subdirectory %q", name)
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return r, err
	}
	parent := filepath.Dir(root)
	if parent == root {
		return r, fmt.Errorf("the googet root %q is the root of a volume; use a directory under it", root)
	}
	dirs := ancestorDirs(root)
	var firstMissing int
	var deepest string
	if err := runWithPrivileges(func() error {
		var err error
		firstMissing, deepest, r.Warnings, err = checkExistingAncestors(dirs)
		return err
	}); err != nil {
		return r, err
	}
	if err := checkVolume(deepest); err != nil {
		return r, err
	}
	if firstMissing < len(dirs) {
		if err := os.MkdirAll(parent, 0774); err != nil {
			return r, err
		}
		if err := runWithPrivileges(func() error {
			for _, dir := range dirs[firstMissing:] {
				risks, err := checkAncestor(dir)
				if err != nil {
					return err
				}
				if len(risks) > 0 {
					r.Warnings = append(r.Warnings, ancestorWarning(dir, risks))
				}
			}
			return nil
		}); err != nil {
			return r, err
		}
	}
	if r.Created, err = createRoot(root); err != nil {
		return r, fmt.Errorf("creating the googet root %q: %w", root, err)
	}
	err = runWithPrivileges(func() error { return secureRoot(root, subdirs, &r) })
	return r, err
}

// createRoot creates the root with rootSDDL. It reports false if something
// already exists at the root's path.
func createRoot(root string) (bool, error) {
	sd, err := windows.SecurityDescriptorFromString(rootSDDL)
	if err != nil {
		return false, err
	}
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return false, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	err = windows.CreateDirectory(p, &sa)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false, nil
	}
	return err == nil, err
}

// secureRoot checks and repairs the root and then its subdirectories, holding
// the root open throughout.
func secureRoot(root string, subdirs []string, r *RootReport) error {
	h, err := openNoFollow(root, rootAccess, shareNoDelete)
	if err != nil {
		return openError("the googet root", root, err)
	}
	defer windows.CloseHandle(h)
	isDir, isLink, err := fileType(h)
	if err != nil {
		return fmt.Errorf("reading the attributes of the googet root %q: %w", root, err)
	}
	if isLink {
		return fmt.Errorf("the googet root %q is a junction, symbolic link or other reparse point; remove it, or use a directory as the root", root)
	}
	if !isDir {
		return fmt.Errorf("the googet root %q isn't a directory; remove it, or use another root", root)
	}
	s, err := readState(h)
	if err != nil {
		return fmt.Errorf("reading the permissions of the googet root %q: %w", root, err)
	}
	recorded := make(map[string]bool)
	if plan := classifyRoot(s, trustedOwner); plan.needed() {
		// Lock the root itself with a temporary protected DACL (tempLockDACL)
		// via SetKernelObjectSecurity before reading children, so no untrusted
		// principal can modify or rename entries while their pre-repair
		// permissions are recorded. Its canonical ACEs aren't inheritable and
		// it has no AI flag, so any crash before repairRoot still triggers a
		// full repair and propagation on retry. Custom read-only ACEs keep
		// their inheritance flags so they survive an interrupted repair, and
		// SetKernelObjectSecurity doesn't propagate them to existing children.
		if err := lockRootNoPropagate(h, plan); err != nil {
			return fmt.Errorf("resetting the permissions of the googet root %q: %w; run googet as an administrator or SYSTEM, or remove the root and run googet again", root, err)
		}
		if plan.ResetOwner {
			r.OwnerReset = true
		}
		// The repair below replaces the ACEs that entries under the root
		// inherit, so CheckEntries uses what they were before it.
		if err := recordEntries(root, h, subdirs, recorded, r); err != nil {
			return err
		}
		if err := repairRoot(h, plan); err != nil {
			return fmt.Errorf("resetting the permissions of the googet root %q: %w; run googet as an administrator or SYSTEM, or remove the root and run googet again", root, err)
		}
		r.DACLReplaced = plan.ReplaceDACL
		s, err := readState(h)
		if err != nil {
			return fmt.Errorf("reading back the permissions of the googet root %q: %w", root, err)
		}
		if classifyRoot(s, trustedOwner).needed() {
			return fmt.Errorf("the permissions of the googet root %q are still not the expected ones after the repair; remove the root and run googet again", root)
		}
	}
	for _, name := range subdirs {
		if err := secureSubdir(filepath.Join(root, name), recorded, r); err != nil {
			return err
		}
	}
	return nil
}

// lockRootNoPropagate applies plan's owner reset and/or a temporary protected
// DACL (tempLockDACL) to h itself via SetKernelObjectSecurity, without
// propagating inheritance to existing children.
func lockRootNoPropagate(h windows.Handle, plan rootPlan) error {
	var info windows.SECURITY_INFORMATION
	var sddl strings.Builder
	if plan.ResetOwner {
		sddl.WriteString("O:BA")
		info |= windows.OWNER_SECURITY_INFORMATION
	}
	if plan.ReplaceDACL {
		d, err := tempLockDACL(plan.DACL)
		if err != nil {
			return err
		}
		sddl.WriteString(d)
		info |= windows.DACL_SECURITY_INFORMATION
	}
	sd, err := windows.SecurityDescriptorFromString(sddl.String())
	if err != nil {
		return err
	}
	return windows.SetKernelObjectSecurity(h, info, sd)
}

// lockDirNoPropagate sets the temporary non-inheritable protected owner and
// DACL (tempLockSDDL) on h itself without propagating inheritance to existing
// children. Because tempLockSDDL omits OICI, subdirNeedsReset still returns
// true if the process exits before resetSubdir completes.
func lockDirNoPropagate(h windows.Handle) error {
	sd, err := windows.SecurityDescriptorFromString(tempLockSDDL)
	if err != nil {
		return err
	}
	return windows.SetKernelObjectSecurity(h, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION, sd)
}

func repairRoot(h windows.Handle, plan rootPlan) error {
	var info windows.SECURITY_INFORMATION
	var owner *windows.SID
	var dacl *windows.ACL
	if plan.ResetOwner {
		var err error
		if owner, err = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err != nil {
			return err
		}
		info |= windows.OWNER_SECURITY_INFORMATION
	}
	if plan.ReplaceDACL {
		sddl, err := daclSDDL(plan.DACL)
		if err != nil {
			return err
		}
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			return err
		}
		if dacl, _, err = sd.DACL(); err != nil {
			return err
		}
		info |= windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	return writeSecurity(h, info, owner, dacl)
}

// secureSubdir removes path if it is a reparse point, and resets its owner and
// DACL to inherit from the root if they aren't trusted. A missing path is
// left alone. Normal subdirectories are opened with rootAccess, omitting
// windows.DELETE so another process holding a non-FILE_SHARE_DELETE handle
// (such as a working directory or enumeration handle) doesn't cause
// ERROR_SHARING_VIOLATION.
func secureSubdir(path string, recorded map[string]bool, r *RootReport) error {
	h, err := openNoFollow(path, rootAccess, shareNoDelete)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return openError("the googet directory", path, err)
	}
	isDir, isLink, err := fileType(h)
	if err != nil {
		windows.CloseHandle(h)
		return fmt.Errorf("reading the attributes of %q: %w", path, err)
	}
	if isLink {
		// Close h before reopening with windows.DELETE because h omits
		// FILE_SHARE_DELETE and would otherwise cause ERROR_SHARING_VIOLATION.
		windows.CloseHandle(h)
		removed, err := removeSubdirLink(path)
		if err != nil {
			return err
		}
		if removed {
			r.RemovedLinks = append(r.RemovedLinks, path)
		}
		return nil
	}
	defer windows.CloseHandle(h)
	if !isDir {
		return fmt.Errorf("%q isn't a directory; remove it and run googet again", path)
	}
	s, err := readState(h)
	if err != nil {
		return fmt.Errorf("reading the permissions of %q: %w", path, err)
	}
	if !subdirNeedsReset(s, trustedOwner) {
		return nil
	}
	// If recordEntries didn't already lock and record this subdirectory during
	// a root repair, lock it without propagating inheritance before recording
	// its children.
	if !recorded[entryKey(path)] {
		if err := lockDirNoPropagate(h); err != nil {
			return fmt.Errorf("resetting the permissions of %q: %w; remove it and run googet again", path, err)
		}
		if err := recordDir(path, h, recorded, r); err != nil {
			return err
		}
	}
	if err := resetSubdir(h); err != nil {
		return fmt.Errorf("resetting the permissions of %q: %w; remove it and run googet again", path, err)
	}
	r.RepairedSubdirs = append(r.RepairedSubdirs, path)
	if s, err = readState(h); err != nil {
		return fmt.Errorf("reading back the permissions of %q: %w", path, err)
	}
	if subdirNeedsReset(s, trustedOwner) || !inheritsCanonical(s) {
		return fmt.Errorf("the permissions of %q are still not the expected ones after the repair; remove it and run googet again", path)
	}
	return nil
}

// removeSubdirLink reopens path for deletion without following reparse points,
// verifies that it is still a reparse point, and removes the link itself. It
// reports whether a link was removed.
func removeSubdirLink(path string) (bool, error) {
	h, err := openNoFollow(path, linkDeleteAccess, shareNoDelete)
	if notFound(err) {
		return false, nil
	}
	if err != nil {
		return false, openError("the link", path, err)
	}
	defer windows.CloseHandle(h)
	attrs, err := fileAttributes(h)
	if err != nil {
		return false, fmt.Errorf("reading the attributes of %q: %w", path, err)
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return false, fmt.Errorf("%q changed before the link could be removed; run googet again", path)
	}
	// The handle is the reparse point itself, so this removes the link and
	// never its target.
	if err := deleteOpen(h); err != nil {
		return false, fmt.Errorf("removing the link %q: %w; remove it and run googet again", path, err)
	}
	return true, nil
}

// resetSubdir sets the owner to Administrators and replaces the DACL with an
// empty, unprotected one, so that the directory only has the ACEs it inherits
// from the root.
func resetSubdir(h windows.Handle) error {
	owner, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("empty DACL is NULL")
	}
	return writeSecurity(h, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, owner, dacl)
}

// checkVolume returns an error if the volume of path doesn't keep ACLs.
func checkVolume(path string) error {
	vol, flags, err := volumeFlags(path)
	if err != nil {
		return err
	}
	if flags&windows.FILE_PERSISTENT_ACLS == 0 {
		return fmt.Errorf("the volume %q doesn't support file permissions; use a googet root on an NTFS or ReFS volume", vol)
	}
	return nil
}

// ancestorDirs returns each parent of root, from the volume root down.
func ancestorDirs(root string) []string {
	vol := filepath.VolumeName(root)
	dir := vol + `\`
	dirs := []string{dir}
	rest := strings.Trim(root[len(vol):], `\`)
	parts := strings.Split(rest, `\`)
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		dirs = append(dirs, dir)
	}
	return dirs
}

func ancestorWarning(dir string, risks []string) string {
	return fmt.Sprintf("%q, a parent of the googet root, lets %s delete it or change its permissions; restrict it to administrators", dir, strings.Join(risks, ", "))
}

// checkExistingAncestors checks existing parents of the root from the volume
// root down without following reparse points. It stops at the first missing
// parent so that reparse points and unsupported volumes are refused before
// missing parents are created.
//
// The default descriptors of a volume root and of C:\ProgramData don't
// trigger the warning: the write access they give Users and Authenticated
// Users on the directory itself doesn't include delete, delete child, write
// DAC or write owner, and their broader grants are inherit-only.
func checkExistingAncestors(dirs []string) (firstMissing int, deepest string, warnings []string, err error) {
	for i, dir := range dirs {
		risks, exists, err := checkAncestorIfExists(dir)
		if err != nil {
			return 0, "", nil, err
		}
		if !exists {
			if i == 0 {
				return 0, "", nil, openError("the parent directory", dir, windows.ERROR_PATH_NOT_FOUND)
			}
			return i, deepest, warnings, nil
		}
		deepest = dir
		if len(risks) > 0 {
			warnings = append(warnings, ancestorWarning(dir, risks))
		}
	}
	return len(dirs), deepest, warnings, nil
}

func checkAncestor(dir string) ([]string, error) {
	risks, exists, err := checkAncestorIfExists(dir)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, openError("the parent directory", dir, windows.ERROR_FILE_NOT_FOUND)
	}
	return risks, nil
}

func checkAncestorIfExists(dir string) ([]string, bool, error) {
	h, err := openNoFollow(dir, ancestorAccess, shareAll)
	if notFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, openError("the parent directory", dir, err)
	}
	defer windows.CloseHandle(h)
	isDir, isLink, err := fileType(h)
	if err != nil {
		return nil, true, fmt.Errorf("reading the attributes of %q: %w", dir, err)
	}
	if isLink {
		return nil, true, fmt.Errorf("%q, a parent of the googet root, is a junction, symbolic link or other reparse point; use a googet root whose path has none", dir)
	}
	if !isDir {
		return nil, true, fmt.Errorf("%q, a parent of the googet root, isn't a directory", dir)
	}
	s, err := readState(h)
	if err != nil {
		return nil, true, fmt.Errorf("reading the permissions of %q: %w", dir, err)
	}
	return ancestorRisks(s, trustedOwner), true, nil
}

func openError(what, path string, err error) error {
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return fmt.Errorf("%s %q is open in another program that can delete or rename it; close that program and run googet again", what, path)
	}
	return fmt.Errorf("opening %s %q: %w", what, path, err)
}

// openNoFollow opens path itself, even if it is a reparse point.
func openNoFollow(path string, access, share uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(p, access, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// fileAttributes returns the Windows file attributes of the open handle h.
func fileAttributes(h windows.Handle) (uint32, error) {
	var info fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, err
	}
	return info.FileAttributes, nil
}

// fileType reports whether the open file h is a directory and whether it is
// a reparse point.
func fileType(h windows.Handle) (isDir, isLink bool, err error) {
	attrs, err := fileAttributes(h)
	if err != nil {
		return false, false, err
	}
	return attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0, attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

func readState(h windows.Handle) (aclState, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return aclState{}, err
	}
	return stateOf(sd)
}

// stateOf converts a security descriptor with an owner and a DACL.
func stateOf(sd *windows.SECURITY_DESCRIPTOR) (aclState, error) {
	var s aclState
	owner, _, err := sd.Owner()
	if err != nil {
		return s, err
	}
	if owner != nil {
		s.OwnerSID = owner.String()
	}
	control, _, err := sd.Control()
	if err != nil {
		return s, err
	}
	s.Protected = control&windows.SE_DACL_PROTECTED != 0
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		// A missing DACL grants everyone full access, like a NULL DACL.
		s.NullDACL = true
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if dacl == nil {
		s.NullDACL = true
		return s, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return s, err
		}
		a := aceInfo{Type: ace.Header.AceType, Flags: ace.Header.AceFlags, Mask: uint32(ace.Mask)}
		switch a.Type {
		case aceTypeAccessAllowed, aceTypeAccessDenied, aceTypeAccessAllowedCallback, aceTypeAccessDeniedCallback:
			// These types have the SID right after the mask.
			a.SID = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		}
		s.ACEs = append(s.ACEs, a)
	}
	return s, nil
}

var trustedOwners struct {
	once sync.Once
	sids map[string]bool
}

// trustedOwner reports whether sid may own the root: Administrators, SYSTEM,
// TrustedInstaller, the user of the process token, or a direct member of the
// local Administrators group. An owner that can't be confirmed, for example a
// member through a domain group, is reset to Administrators, which is
// harmless.
func trustedOwner(sid string) bool {
	if trustedSIDs[sid] {
		return true
	}
	trustedOwners.once.Do(func() {
		trustedOwners.sids = make(map[string]bool)
		if u, err := windows.GetCurrentProcessToken().GetTokenUser(); err == nil {
			trustedOwners.sids[u.User.Sid.String()] = true
		}
		if members, err := localAdministrators(); err == nil {
			for _, m := range members {
				trustedOwners.sids[m] = true
			}
		}
	})
	return trustedOwners.sids[sid]
}

// localAdministrators returns the SIDs of the direct members of the local
// Administrators group. The group is looked up by its SID; its name, which is
// localized, is only passed back to NetLocalGroupGetMembers.
func localAdministrators() ([]string, error) {
	if err := procNetLocalGroupGetMembers.Find(); err != nil {
		return nil, err
	}
	ba, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	name, _, _, err := ba.LookupAccount("")
	if err != nil {
		return nil, err
	}
	group, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var sids []string
	var resume uintptr
	for {
		var buf *byte
		var read, total uint32
		r1, _, _ := procNetLocalGroupGetMembers.Call(
			0,
			uintptr(unsafe.Pointer(group)),
			0, // LOCALGROUP_MEMBERS_INFO_0, which holds only the SID.
			uintptr(unsafe.Pointer(&buf)),
			maxPreferredLength,
			uintptr(unsafe.Pointer(&read)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&resume)),
		)
		status := windows.Errno(r1)
		if status != 0 && status != windows.ERROR_MORE_DATA {
			return nil, fmt.Errorf("NetLocalGroupGetMembers: %w", status)
		}
		if buf != nil {
			for _, sid := range unsafe.Slice((**windows.SID)(unsafe.Pointer(buf)), read) {
				sids = append(sids, sid.String())
			}
			windows.NetApiBufferFree(buf)
		}
		if status == 0 {
			return sids, nil
		}
	}
}

// withPrivileges runs f with repairPrivileges enabled. It enables them on a
// thread token rather than on the process token: the goroutine is locked to
// its thread, which impersonates a copy of the process token, so no other
// goroutine runs with these privileges and the process token never changes.
// The previous privilege state is restored before the impersonation ends.
func withPrivileges(f func() error) (err error) {
	runtime.LockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("ImpersonateSelf: %w", err)
	}
	defer func() {
		if rerr := windows.RevertToSelf(); rerr != nil {
			// The thread stays locked, so no other goroutine runs on it
			// with the impersonation token.
			if err == nil {
				err = fmt.Errorf("RevertToSelf: %w", rerr)
			}
			return
		}
		runtime.UnlockOSThread()
	}()
	var tok windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, true, &tok); err != nil {
		return fmt.Errorf("OpenThreadToken: %w", err)
	}
	defer tok.Close()
	enable := tokenPrivileges{PrivilegeCount: uint32(len(repairPrivileges))}
	for i, name := range repairPrivileges {
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return err
		}
		if err := windows.LookupPrivilegeValue(nil, p, &enable.Privileges[i].Luid); err != nil {
			return fmt.Errorf("LookupPrivilegeValue(%s): %w", name, err)
		}
		enable.Privileges[i].Attributes = windows.SE_PRIVILEGE_ENABLED
	}
	// A privilege that the token doesn't hold is skipped; an open that needs
	// it fails later with a clear error.
	var prev tokenPrivileges
	var prevLen uint32
	if err := windows.AdjustTokenPrivileges(tok, false, (*windows.Tokenprivileges)(unsafe.Pointer(&enable)), uint32(unsafe.Sizeof(prev)), (*windows.Tokenprivileges)(unsafe.Pointer(&prev)), &prevLen); err != nil {
		return fmt.Errorf("AdjustTokenPrivileges: %w", err)
	}
	defer func() {
		if rerr := windows.AdjustTokenPrivileges(tok, false, (*windows.Tokenprivileges)(unsafe.Pointer(&prev)), 0, nil, nil); rerr != nil && err == nil {
			err = fmt.Errorf("restoring privileges: %w", rerr)
		}
	}()
	return f()
}
