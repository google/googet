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
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/sys/windows"
)

// usersModifySDDL is a protected DACL that also gives Users modify access.
const usersModifySDDL = "D:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)(A;OICI;0x1301bf;;;BU)"

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
	procAccessCheck           = advapi32.NewProc("AccessCheck")

	testSubdirs = []string{"repos", "cache"}
	sortACEs    = cmpopts.SortSlices(func(a, b aceInfo) bool { return fmt.Sprint(a) < fmt.Sprint(b) })
)

type genericMapping struct {
	GenericRead    uint32
	GenericWrite   uint32
	GenericExecute uint32
	GenericAll     uint32
}

type privilegeSet struct {
	PrivilegeCount uint32
	Control        uint32
	Privilege      [16]windows.LUIDAndAttributes
}

type elevationTB interface {
	Helper()
	Fatal(args ...any)
	Skip(args ...any)
}

// checkElevation skips the test unless elevated is true. When isCI is true, it
// fails instead so CI runs can't pass without executing elevated checks.
func checkElevation(tb elevationTB, elevated, isCI bool) {
	tb.Helper()
	if elevated {
		return
	}
	if isCI {
		tb.Fatal("this test needs an elevated process")
		return
	}
	tb.Skip("this test needs an elevated process")
}

// requireElevated skips the test unless the process is elevated. On GitHub
// Actions, whose Windows runners are elevated, it fails instead.
func requireElevated(t *testing.T) {
	t.Helper()
	checkElevation(t, windows.GetCurrentProcessToken().IsElevated(), os.Getenv("GITHUB_ACTIONS") != "")
}

// filteredUserToken returns an impersonation token derived from the current
// process token with BUILTIN\Administrators marked deny-only and privileges
// removed. It models a non-administrator principal independently of stateOf.
func filteredUserToken(t *testing.T) windows.Token {
	t.Helper()
	var procTok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &procTok); err != nil {
		t.Fatalf("OpenProcessToken: %v", err)
	}
	defer procTok.Close()
	ba, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(BA): %v", err)
	}
	disable := []windows.SIDAndAttributes{{Sid: ba}}
	const disableMaxPrivilege = 0x1
	var restricted windows.Token
	r1, _, e1 := procCreateRestrictedToken.Call(
		uintptr(procTok),
		disableMaxPrivilege,
		uintptr(len(disable)),
		uintptr(unsafe.Pointer(&disable[0])),
		0, 0, 0, 0,
		uintptr(unsafe.Pointer(&restricted)),
	)
	if r1 == 0 {
		t.Fatalf("CreateRestrictedToken: %v", e1)
	}
	defer restricted.Close()
	var imp windows.Token
	if err := windows.DuplicateTokenEx(restricted, windows.TOKEN_IMPERSONATE|windows.TOKEN_QUERY, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &imp); err != nil {
		t.Fatalf("DuplicateTokenEx: %v", err)
	}
	t.Cleanup(func() { imp.Close() })
	return imp
}

// checkAccess evaluates AccessCheck for path using clientToken and desiredAccess.
func checkAccess(t *testing.T, path string, clientToken windows.Token, desiredAccess uint32) bool {
	t.Helper()
	var sd *windows.SECURITY_DESCRIPTOR
	err := withPrivileges(func() error {
		h, err := openNoFollow(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		sd, err = windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		return err
	})
	if err != nil {
		t.Fatalf("reading descriptor of %q for AccessCheck: %v", path, err)
	}
	gm := genericMapping{
		GenericRead:    windows.FILE_GENERIC_READ,
		GenericWrite:   windows.FILE_GENERIC_WRITE,
		GenericExecute: windows.FILE_GENERIC_EXECUTE,
		GenericAll:     accessFileAllAccess,
	}
	var privs privilegeSet
	privLen := uint32(unsafe.Sizeof(privs))
	var granted uint32
	var status int32
	r1, _, e1 := procAccessCheck.Call(
		uintptr(unsafe.Pointer(sd)),
		uintptr(clientToken),
		uintptr(desiredAccess),
		uintptr(unsafe.Pointer(&gm)),
		uintptr(unsafe.Pointer(&privs)),
		uintptr(unsafe.Pointer(&privLen)),
		uintptr(unsafe.Pointer(&granted)),
		uintptr(unsafe.Pointer(&status)),
	)
	if r1 == 0 {
		t.Fatalf("AccessCheck(%q, %#x): %v", path, desiredAccess, e1)
	}
	return status != 0
}

// checkFilteredAccess verifies via Windows AccessCheck that a non-admin token
// is denied write and metadata-mutation rights on path while keeping read and
// execute access.
func checkFilteredAccess(t *testing.T, tok windows.Token, path string) {
	t.Helper()
	denied := []struct {
		name string
		mask uint32
	}{
		{"FILE_WRITE_DATA", windows.FILE_WRITE_DATA},
		{"FILE_APPEND_DATA", windows.FILE_APPEND_DATA},
		{"FILE_WRITE_EA", windows.FILE_WRITE_EA},
		{"FILE_WRITE_ATTRIBUTES", windows.FILE_WRITE_ATTRIBUTES},
		{"FILE_DELETE_CHILD", accessFileDeleteChild},
		{"DELETE", windows.DELETE},
		{"WRITE_DAC", windows.WRITE_DAC},
		{"WRITE_OWNER", windows.WRITE_OWNER},
	}
	for _, d := range denied {
		if checkAccess(t, path, tok, d.mask) {
			t.Errorf("AccessCheck(%q, %s) allowed under filtered token, want denied", path, d.name)
		}
	}
	if !checkAccess(t, path, tok, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_EXECUTE) {
		t.Errorf("AccessCheck(%q, READ|EXECUTE) denied under filtered token, want allowed", path)
	}
}

// descriptor returns the owner and DACL of path itself, and the bytes of its
// owner, group and DACL.
func descriptor(t *testing.T, path string) (aclState, []byte) {
	t.Helper()
	var s aclState
	var b []byte
	err := withPrivileges(func() error {
		h, err := openNoFollow(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		b = bytes.Clone(unsafe.Slice((*byte)(unsafe.Pointer(sd)), sd.Length()))
		s, err = stateOf(sd)
		return err
	})
	if err != nil {
		t.Fatalf("reading the descriptor of %q: %v", path, err)
	}
	return s, b
}

// setDescriptor sets the owner and DACL of path from sddl, each only if sddl
// has it.
func setDescriptor(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString(%q): %v", sddl, err)
	}
	var info windows.SECURITY_INFORMATION
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		info |= windows.OWNER_SECURITY_INFORMATION
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err == nil {
		info |= windows.DACL_SECURITY_INFORMATION
		if control&windows.SE_DACL_PROTECTED != 0 {
			info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
		} else {
			info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
		}
	}
	if err := withPrivileges(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil)
	}); err != nil {
		t.Fatalf("setting %q on %q: %v", sddl, path, err)
	}
}

func mklink(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %q %q: %v\n%s", link, target, err, out)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0777); err != nil {
		t.Fatal(err)
	}
}

func prepareRoot(t *testing.T, root string) RootReport {
	t.Helper()
	r, err := PrepareRoot(root, testSubdirs)
	if err != nil {
		t.Fatalf("PrepareRoot(%q) failed: %v", root, err)
	}
	return r
}

// checkCanonicalRoot checks that root has a trusted owner, wantOwner if set,
// and exactly the canonical protected DACL plus extra.
func checkCanonicalRoot(t *testing.T, root, wantOwner string, extra ...aceInfo) {
	t.Helper()
	s, _ := descriptor(t, root)
	if wantOwner != "" && s.OwnerSID != wantOwner {
		t.Errorf("owner of %q = %s, want %s", root, s.OwnerSID, wantOwner)
	}
	if !trustedOwner(s.OwnerSID) {
		t.Errorf("owner of %q = %s, want a trusted owner", root, s.OwnerSID)
	}
	if s.NullDACL || !s.Protected {
		t.Errorf("DACL of %q: NULL = %v, protected = %v; want a protected DACL", root, s.NullDACL, s.Protected)
	}
	if diff := cmp.Diff(withACEs(extra...), s.ACEs, sortACEs); diff != "" {
		t.Errorf("ACEs of %q mismatch (-want +got):\n%s", root, diff)
	}
}

func checkUnchanged(t *testing.T, path string, before []byte) {
	t.Helper()
	if _, after := descriptor(t, path); !bytes.Equal(before, after) {
		t.Errorf("descriptor of %q changed", path)
	}
}

func TestRootSDDLMatchesCanonicalACEs(t *testing.T) {
	sd, err := windows.SecurityDescriptorFromString(rootSDDL)
	if err != nil {
		t.Fatal(err)
	}
	s, err := stateOf(sd)
	if err != nil {
		t.Fatal(err)
	}
	want := aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: canonicalACEs}
	if diff := cmp.Diff(want, s); diff != "" {
		t.Errorf("rootSDDL mismatch (-want +got):\n%s", diff)
	}
}

func TestPrepareRootRejectsInvalidSubdir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	for _, name := range []string{"", ".", "..", `..\x`, "a/b", "C:x"} {
		if _, err := PrepareRoot(root, []string{name}); err == nil {
			t.Errorf("PrepareRoot(%q, [%q]) succeeded, want an error", root, name)
		} else if !strings.Contains(err.Error(), "invalid googet root subdirectory") {
			t.Errorf("PrepareRoot(%q, [%q]) error = %q, want invalid subdirectory message", root, name, err)
		}
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("PrepareRoot with an invalid subdirectory created %q (Lstat error: %v)", root, err)
	}
}

func TestPrepareRootCreatesProtectedRoot(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "a", "b", "googet")
	r := prepareRoot(t, root)
	if !r.Created || r.Changed() {
		t.Errorf("PrepareRoot(%q) = %+v, want Created and no change", root, r)
	}
	checkCanonicalRoot(t, root, sidAdministrators)

	// Verify inherited ACLs via AccessCheck on the root, subdirectories and a
	// newly created file.
	repos := filepath.Join(root, "repos")
	cache := filepath.Join(root, "cache")
	mkdir(t, repos)
	mkdir(t, cache)
	sampleFile := filepath.Join(repos, "sample.repo")
	if err := os.WriteFile(sampleFile, []byte("test"), 0666); err != nil {
		t.Fatal(err)
	}
	tok := filteredUserToken(t)
	for _, p := range []string{root, repos, cache, sampleFile} {
		checkFilteredAccess(t, tok, p)
	}
}

func TestPrepareRootRepairsInheritedACL(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	if s, _ := descriptor(t, root); s.Protected {
		t.Fatalf("precondition: %q has a protected DACL", root)
	}
	r := prepareRoot(t, root)
	if r.Created || !r.DACLReplaced || !r.Changed() {
		t.Errorf("PrepareRoot(%q) = %+v, want the DACL replaced", root, r)
	}
	checkCanonicalRoot(t, root, "")
}

func TestPrepareRootRepairsUsersWriteACE(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	setDescriptor(t, root, usersModifySDDL)
	r := prepareRoot(t, root)
	if !r.DACLReplaced {
		t.Errorf("PrepareRoot(%q) = %+v, want the DACL replaced", root, r)
	}
	checkCanonicalRoot(t, root, "")
}

func TestPrepareRootRepairsNullDACL(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	setDescriptor(t, root, "D:PNO_ACCESS_CONTROL")
	if s, _ := descriptor(t, root); !s.NullDACL {
		t.Fatalf("precondition: %q doesn't have a NULL DACL: %+v", root, s)
	}
	r := prepareRoot(t, root)
	if !r.DACLReplaced {
		t.Errorf("PrepareRoot(%q) = %+v, want the DACL replaced", root, r)
	}
	checkCanonicalRoot(t, root, "")
}

func TestPrepareRootResetsUntrustedOwner(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	setDescriptor(t, root, "O:BU"+strings.TrimPrefix(rootSDDL, "O:BAG:SY"))
	if s, _ := descriptor(t, root); s.OwnerSID != sidUsers {
		t.Fatalf("precondition: owner of %q = %s, want %s", root, s.OwnerSID, sidUsers)
	}
	r := prepareRoot(t, root)
	if !r.OwnerReset || r.DACLReplaced {
		t.Errorf("PrepareRoot(%q) = %+v, want only the owner reset", root, r)
	}
	checkCanonicalRoot(t, root, sidAdministrators)
}

func TestPrepareRootPreservesReadOnlyACE(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	setDescriptor(t, root, "D:AI(A;OICI;0x1200a9;;;WD)(A;OICI;0x1301bf;;;AU)")
	r := prepareRoot(t, root)
	if !r.DACLReplaced {
		t.Errorf("PrepareRoot(%q) = %+v, want the DACL replaced", root, r)
	}
	checkCanonicalRoot(t, root, "", allow(oici, readMask, sidEveryone))
}

func TestPrepareRootRefusesJunction(t *testing.T) {
	requireElevated(t)
	target := t.TempDir()
	setDescriptor(t, target, usersModifySDDL)
	_, before := descriptor(t, target)
	root := filepath.Join(t.TempDir(), "googet")
	mklink(t, root, target)
	if r, err := PrepareRoot(root, testSubdirs); err == nil {
		t.Errorf("PrepareRoot(%q) = %+v, want an error for a junction", root, r)
	} else if !strings.Contains(err.Error(), "reparse point") {
		t.Errorf("PrepareRoot(%q) error = %q, want reparse point message", root, err)
	}
	checkUnchanged(t, target, before)
	for _, name := range testSubdirs {
		if _, err := os.Lstat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Errorf("PrepareRoot created %q in the junction target (Lstat error: %v)", name, err)
		}
	}
}

func TestPrepareRootRefusesJunctionParent(t *testing.T) {
	requireElevated(t)
	target := t.TempDir()
	_, before := descriptor(t, target)
	link := filepath.Join(t.TempDir(), "link")
	mklink(t, link, target)
	root := filepath.Join(link, "googet")
	if r, err := PrepareRoot(root, testSubdirs); err == nil {
		t.Errorf("PrepareRoot(%q) = %+v, want an error for a junction parent", root, r)
	} else if !strings.Contains(err.Error(), "reparse point") {
		t.Errorf("PrepareRoot(%q) error = %q, want reparse point message", root, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "googet")); !os.IsNotExist(err) {
		t.Errorf("PrepareRoot created the root in the junction target (Lstat error: %v)", err)
	}
	checkUnchanged(t, target, before)
}

func TestPrepareRootRefusesFile(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	if err := os.WriteFile(root, []byte("data"), 0666); err != nil {
		t.Fatal(err)
	}
	_, before := descriptor(t, root)
	if r, err := PrepareRoot(root, testSubdirs); err == nil {
		t.Errorf("PrepareRoot(%q) = %+v, want an error for a file", root, r)
	} else if !strings.Contains(err.Error(), "isn't a directory") {
		t.Errorf("PrepareRoot(%q) error = %q, want non-directory message", root, err)
	}
	if got, err := os.ReadFile(root); err != nil || string(got) != "data" {
		t.Errorf("ReadFile(%q) = %q, %v; want the file unchanged", root, got, err)
	}
	checkUnchanged(t, root, before)
}

func TestPrepareRootRefusesVolumeRoot(t *testing.T) {
	requireElevated(t)
	vol := filepath.VolumeName(t.TempDir()) + `\`
	if r, err := PrepareRoot(vol, nil); err == nil {
		t.Errorf("PrepareRoot(%q) = %+v, want an error for a volume root", vol, r)
	} else if !strings.Contains(err.Error(), "root of a volume") {
		t.Errorf("PrepareRoot(%q) error = %q, want volume root message", vol, err)
	}
}

func TestPrepareRootRemovesSubdirJunction(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	prepareRoot(t, root)
	target := t.TempDir()
	sentinel := filepath.Join(target, "sentinel")
	if err := os.WriteFile(sentinel, nil, 0666); err != nil {
		t.Fatal(err)
	}
	_, before := descriptor(t, target)
	repos := filepath.Join(root, "repos")
	mklink(t, repos, target)
	r := prepareRoot(t, root)
	if diff := cmp.Diff([]string{repos}, r.RemovedLinks); diff != "" || !r.Changed() {
		t.Errorf("PrepareRoot(%q) = %+v, want %q removed; RemovedLinks mismatch (-want +got):\n%s", root, r, repos, diff)
	}
	if _, err := os.Lstat(repos); !os.IsNotExist(err) {
		t.Errorf("%q still exists (Lstat error: %v)", repos, err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("junction target lost its contents: %v", err)
	}
	checkUnchanged(t, target, before)
}

func TestPrepareRootRemovesSubdirReadOnlyFileSymlink(t *testing.T) {
	requireElevated(t)
	for _, tc := range []struct {
		desc       string
		extraAttrs uint32
	}{
		{desc: "with archive attribute", extraAttrs: windows.FILE_ATTRIBUTE_ARCHIVE},
		{desc: "readonly only"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			prepareRoot(t, root)
			target := filepath.Join(t.TempDir(), "target.txt")
			if err := os.WriteFile(target, []byte("target data"), 0666); err != nil {
				t.Fatal(err)
			}
			_, before := descriptor(t, target)
			repos := filepath.Join(root, "repos")
			if err := os.Symlink(target, repos); err != nil {
				t.Fatalf("os.Symlink(%q, %q): %v", target, repos, err)
			}
			// Set FILE_ATTRIBUTE_READONLY on the symlink itself rather than
			// its target.
			lh, err := openNoFollow(repos, windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
			if err != nil {
				t.Fatalf("opening symlink %q: %v", repos, err)
			}
			basic := fileBasicInfo{FileAttributes: windows.FILE_ATTRIBUTE_READONLY | tc.extraAttrs}
			err = windows.SetFileInformationByHandle(lh, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)))
			var attrs uint32
			if err == nil {
				attrs, err = fileAttributes(lh)
			}
			windows.CloseHandle(lh)
			if err != nil {
				t.Fatalf("setting FILE_ATTRIBUTE_READONLY on symlink %q: %v", repos, err)
			}
			if attrs&windows.FILE_ATTRIBUTE_READONLY == 0 || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 || attrs&settableFileAttrs&^windows.FILE_ATTRIBUTE_READONLY != tc.extraAttrs {
				t.Fatalf("precondition: symlink %q attributes = %#x, want READONLY, REPARSE_POINT and other settable attributes %#x", repos, attrs, tc.extraAttrs)
			}

			r := prepareRoot(t, root)
			if diff := cmp.Diff([]string{repos}, r.RemovedLinks); diff != "" || !r.Changed() {
				t.Errorf("PrepareRoot(%q) = %+v, want %q removed; RemovedLinks mismatch (-want +got):\n%s", root, r, repos, diff)
			}
			if _, err := os.Lstat(repos); !os.IsNotExist(err) {
				t.Errorf("%q still exists (Lstat error: %v)", repos, err)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "target data" {
				t.Errorf("ReadFile(%q) = %q, %v; want target unchanged", target, got, err)
			}
			checkUnchanged(t, target, before)
		})
	}
}

func TestPrepareRootAllowsOpenSubdirWithoutDeleteShare(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	prepareRoot(t, root)
	repos := filepath.Join(root, "repos")
	mkdir(t, repos)

	// Hold a read handle on repos without FILE_SHARE_DELETE, matching a
	// working-directory or directory-enumeration handle held by another
	// process.
	h, err := openNoFollow(repos, windows.FILE_LIST_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		t.Fatalf("opening FILE_LIST_DIRECTORY handle on %q: %v", repos, err)
	}
	defer windows.CloseHandle(h)

	if r := prepareRoot(t, root); r.Changed() {
		t.Errorf("PrepareRoot(%q) on healthy open subdir = %+v, want no change", root, r)
	}

	// Repairing an untrusted subdirectory DACL should also succeed while the
	// non-DELETE handle is open.
	setDescriptor(t, repos, "D:AI(A;OICI;0x1301bf;;;BU)")
	r := prepareRoot(t, root)
	if diff := cmp.Diff([]string{repos}, r.RepairedSubdirs); diff != "" {
		t.Errorf("PrepareRoot(%q) RepairedSubdirs mismatch (-want +got):\n%s", root, diff)
	}
}

func TestPrepareRootResetsSubdirACL(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	prepareRoot(t, root)
	repos := filepath.Join(root, "repos")
	mkdir(t, repos)
	setDescriptor(t, repos, "D:AI(A;OICI;0x1301bf;;;BU)")
	if s, _ := descriptor(t, repos); !subdirNeedsReset(s, trustedOwner) {
		t.Fatalf("precondition: %q doesn't need a reset: %+v", repos, s)
	}
	r := prepareRoot(t, root)
	if diff := cmp.Diff([]string{repos}, r.RepairedSubdirs); diff != "" {
		t.Errorf("PrepareRoot(%q) RepairedSubdirs mismatch (-want +got):\n%s", root, diff)
	}
	s, _ := descriptor(t, repos)
	var want []aceInfo
	for _, a := range canonicalACEs {
		a.Flags |= aceFlagInherited
		want = append(want, a)
	}
	if s.OwnerSID != sidAdministrators || s.Protected || s.NullDACL {
		t.Errorf("%q: owner %s, protected %v, NULL DACL %v; want owner %s and an inherited DACL", repos, s.OwnerSID, s.Protected, s.NullDACL, sidAdministrators)
	}
	if diff := cmp.Diff(want, s.ACEs, sortACEs); diff != "" {
		t.Errorf("ACEs of %q mismatch (-want +got):\n%s", repos, diff)
	}
}

func TestPrepareRootIsIdempotent(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	prepareRoot(t, root)
	for _, name := range testSubdirs {
		mkdir(t, filepath.Join(root, name))
	}
	prepareRoot(t, root)
	paths := []string{root, filepath.Join(root, "repos"), filepath.Join(root, "cache")}
	var before [][]byte
	var mtimes []time.Time
	for _, p := range paths {
		_, b := descriptor(t, p)
		before = append(before, b)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		mtimes = append(mtimes, fi.ModTime())
	}
	origWrite := writeSecurity
	t.Cleanup(func() { writeSecurity = origWrite })
	writeCalls := 0
	writeSecurity = func(h windows.Handle, info windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
		writeCalls++
		return origWrite(h, info, owner, dacl)
	}
	if r := prepareRoot(t, root); r.Created || r.Changed() || len(r.Warnings) > 0 {
		t.Errorf("PrepareRoot(%q) on a prepared root = %+v, want no change", root, r)
	}
	if writeCalls != 0 {
		t.Errorf("writeSecurity called %d times on an already canonical root, want 0", writeCalls)
	}
	for i, p := range paths {
		checkUnchanged(t, p, before[i])
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(mtimes[i]) {
			t.Errorf("modification time of %q changed from %v to %v", p, mtimes[i], fi.ModTime())
		}
	}
}

func TestPrepareRootHoldsRootDuringRepair(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	target := t.TempDir()
	setDescriptor(t, target, usersModifySDDL)
	_, before := descriptor(t, target)
	moved := root + ".moved"

	orig := writeSecurity
	t.Cleanup(func() { writeSecurity = orig })
	var called bool
	var renameErr error
	writeSecurity = func(h windows.Handle, info windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
		if !called {
			called = true
			if renameErr = os.Rename(root, moved); renameErr == nil {
				if out, err := exec.Command("cmd", "/c", "mklink", "/J", root, target).CombinedOutput(); err != nil {
					t.Errorf("mklink /J %q %q: %v\n%s", root, target, err, out)
				}
			}
		}
		return orig(h, info, owner, dacl)
	}

	r, err := PrepareRoot(root, nil)
	if !called {
		t.Fatalf("PrepareRoot(%q) = %+v, %v; want a repair", root, r, err)
	}
	if renameErr == nil {
		t.Errorf("renaming the root during the repair succeeded, want it to fail while PrepareRoot holds the root")
	} else {
		if err != nil {
			t.Errorf("PrepareRoot(%q) failed: %v", root, err)
		}
		checkCanonicalRoot(t, root, "")
	}
	checkUnchanged(t, target, before)
}

func TestPrepareRootDoesNotPropagateThroughJunction(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	cache := filepath.Join(root, "cache")
	mkdir(t, cache)
	target := t.TempDir()
	sub := filepath.Join(target, "sub")
	mkdir(t, sub)
	_, beforeTarget := descriptor(t, target)
	_, beforeSub := descriptor(t, sub)
	mklink(t, filepath.Join(cache, "link"), target)
	if r := prepareRoot(t, root); !r.DACLReplaced {
		t.Errorf("PrepareRoot(%q) = %+v, want the DACL replaced", root, r)
	}
	checkUnchanged(t, target, beforeTarget)
	checkUnchanged(t, sub, beforeSub)
}

func TestPrepareRootWarnsOnWritableParent(t *testing.T) {
	requireElevated(t)
	parent := filepath.Join(t.TempDir(), "parent")
	mkdir(t, parent)
	setDescriptor(t, parent, "D:AI(A;OICI;FA;;;BU)")
	root := filepath.Join(parent, "googet")
	r := prepareRoot(t, root)
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], fmt.Sprintf("%q", parent)) || !strings.Contains(r.Warnings[0], sidUsers) {
		t.Errorf("PrepareRoot(%q) warnings = %q, want one about %q and %s", root, r.Warnings, parent, sidUsers)
	}
	checkCanonicalRoot(t, root, sidAdministrators)
}

func TestPrepareRootNoWarningsUnderProgramData(t *testing.T) {
	requireElevated(t)
	pd := os.Getenv("ProgramData")
	if pd == "" {
		t.Fatal("ProgramData isn't set")
	}
	root := filepath.Join(pd, fmt.Sprintf("googet-test-%d", time.Now().UnixNano()))
	t.Cleanup(func() { os.RemoveAll(root) })
	r := prepareRoot(t, root)
	if len(r.Warnings) > 0 {
		t.Errorf("PrepareRoot(%q) warnings = %q, want none", root, r.Warnings)
	}
	checkCanonicalRoot(t, root, sidAdministrators)
}

func TestPrepareRootRepairsProgramDataLikeTree(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	repos := filepath.Join(root, "repos")
	cache := filepath.Join(root, "cache")
	mkdir(t, repos)
	mkdir(t, cache)

	// Simulate an unprotected root owned by Users with ProgramData-like grants
	// and pre-existing subdirectories with untrusted owners and explicit write
	// ACEs.
	const programDataLikeSDDL = "O:BUD:AI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICIIO;GA;;;CO)(A;OICI;0x1200a9;;;BU)(A;CI;0x116;;;BU)"
	setDescriptor(t, root, programDataLikeSDDL)
	setDescriptor(t, repos, "O:BUD:AI(A;OICI;0x1301bf;;;BU)")
	setDescriptor(t, cache, "O:BUD:AI(A;OICI;0x1301bf;;;BU)")

	r := prepareRoot(t, root)
	if r.Created || !r.OwnerReset || !r.DACLReplaced || !r.Changed() {
		t.Errorf("PrepareRoot(%q) = %+v, want owner reset and DACL replaced", root, r)
	}
	if diff := cmp.Diff([]string{repos, cache}, r.RepairedSubdirs); diff != "" {
		t.Errorf("RepairedSubdirs mismatch (-want +got):\n%s", diff)
	}
	checkCanonicalRoot(t, root, sidAdministrators)
	tok := filteredUserToken(t)
	for _, p := range []string{root, repos, cache} {
		checkFilteredAccess(t, tok, p)
	}
}

func TestPrepareRootRefusesReparseAndFileVariants(t *testing.T) {
	requireElevated(t)

	t.Run("directory symlink root", func(t *testing.T) {
		target := t.TempDir()
		setDescriptor(t, target, usersModifySDDL)
		_, before := descriptor(t, target)
		root := filepath.Join(t.TempDir(), "googet")
		if out, err := exec.Command("cmd", "/c", "mklink", "/D", root, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /D %q %q: %v\n%s", root, target, err, out)
		}
		if r, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Errorf("PrepareRoot(%q) = %+v, want an error for a directory symlink", root, r)
		} else if !strings.Contains(err.Error(), "reparse point") {
			t.Errorf("PrepareRoot(%q) error = %q, want reparse point message", root, err)
		}
		checkUnchanged(t, target, before)
	})

	t.Run("dangling junction root", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "gone")
		mkdir(t, target)
		root := filepath.Join(t.TempDir(), "googet")
		mklink(t, root, target)
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if r, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Errorf("PrepareRoot(%q) = %+v, want an error for a dangling junction", root, r)
		} else if !strings.Contains(err.Error(), "reparse point") {
			t.Errorf("PrepareRoot(%q) error = %q, want reparse point message", root, err)
		}
	})

	t.Run("missing parent under junction ancestor", func(t *testing.T) {
		target := t.TempDir()
		_, before := descriptor(t, target)
		link := filepath.Join(t.TempDir(), "link")
		mklink(t, link, target)
		root := filepath.Join(link, "sub", "googet")
		if r, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Errorf("PrepareRoot(%q) = %+v, want an error for a junction ancestor", root, r)
		} else if !strings.Contains(err.Error(), "reparse point") {
			t.Errorf("PrepareRoot(%q) error = %q, want reparse point message", root, err)
		}
		if _, err := os.Lstat(filepath.Join(target, "sub")); !os.IsNotExist(err) {
			t.Errorf("PrepareRoot created intermediate directory inside junction target (Lstat error: %v)", err)
		}
		checkUnchanged(t, target, before)
	})

	t.Run("regular file at subdirectory path", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		prepareRoot(t, root)
		repos := filepath.Join(root, "repos")
		if err := os.WriteFile(repos, []byte("file"), 0666); err != nil {
			t.Fatal(err)
		}
		if r, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Errorf("PrepareRoot(%q) = %+v, want an error when repos is a regular file", root, r)
		} else if !strings.Contains(err.Error(), "isn't a directory") {
			t.Errorf("PrepareRoot(%q) error = %q, want non-directory message", root, err)
		}
	})
}

func TestPrepareRootRepairsUntrustedRealACLs(t *testing.T) {
	requireElevated(t)
	const base = "(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)"
	for _, tc := range []struct {
		desc        string
		sddl        string
		wantReplace bool
		extra       []aceInfo
	}{
		{desc: "unprotected canonical", sddl: "D:AI" + base, wantReplace: true},
		{desc: "inherit-only full control for Users", sddl: "D:PAI" + base + "(A;OICIIO;FA;;;BU)", wantReplace: true},
		{desc: "GENERIC_ALL for Users", sddl: "D:PAI" + base + "(A;OICI;GA;;;BU)", wantReplace: true},
		{desc: "WRITE_DAC for Users", sddl: "D:PAI" + base + "(A;OICI;WD;;;BU)", wantReplace: true},
		{desc: "DELETE for Users", sddl: "D:PAI" + base + "(A;OICI;SD;;;BU)", wantReplace: true},
		{desc: "FILE_DELETE_CHILD for Users", sddl: "D:PAI" + base + "(A;OICI;DC;;;BU)", wantReplace: true},
		{desc: "OWNER RIGHTS write DAC", sddl: "D:PAI" + base + "(A;;WD;;;OW)", wantReplace: true},
		{desc: "deny ACE for Administrators", sddl: "D:PAI(D;OICI;FA;;;BA)" + base, wantReplace: true},
		{
			desc:        "extra ACE for TrustedInstaller kept when canonical",
			sddl:        "D:PAI" + base + "(A;OICI;FA;;;" + sidTrustedInstaller + ")",
			wantReplace: false,
			extra:       []aceInfo{allow(oici, accessFileAllAccess, sidTrustedInstaller)},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			mkdir(t, root)
			setDescriptor(t, root, "O:BA"+tc.sddl)
			r := prepareRoot(t, root)
			if r.DACLReplaced != tc.wantReplace {
				t.Errorf("PrepareRoot(%q) DACLReplaced = %v, want %v (report: %+v)", root, r.DACLReplaced, tc.wantReplace, r)
			}
			checkCanonicalRoot(t, root, sidAdministrators, tc.extra...)
		})
	}
}

func TestPrepareRootRepairsRestrictedOwnerRoot(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	// Non-admin owner, protected DACL with no admin ACEs and an OWNER RIGHTS
	// ACE limiting the owner to read-only. Repairing this requires scoped
	// backup/restore/take-ownership privileges.
	setDescriptor(t, root, "O:BUD:PAI(A;OICI;0x1200a9;;;OW)")
	r := prepareRoot(t, root)
	if !r.OwnerReset || !r.DACLReplaced {
		t.Errorf("PrepareRoot(%q) = %+v, want owner reset and DACL replaced", root, r)
	}
	checkCanonicalRoot(t, root, sidAdministrators)
	checkFilteredAccess(t, filteredUserToken(t), root)
}

func hasEnabledPrivilege(tok windows.Token, name string) (bool, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, p, &luid); err != nil {
		return false, fmt.Errorf("LookupPrivilegeValue(%s): %w", name, err)
	}
	var size uint32
	_ = windows.GetTokenInformation(tok, windows.TokenPrivileges, nil, 0, &size)
	if size == 0 {
		return false, errors.New("GetTokenInformation(TokenPrivileges) returned size 0")
	}
	buf := make([]byte, size)
	if err := windows.GetTokenInformation(tok, windows.TokenPrivileges, &buf[0], size, &size); err != nil {
		return false, fmt.Errorf("GetTokenInformation(TokenPrivileges): %w", err)
	}
	tp := (*windows.Tokenprivileges)(unsafe.Pointer(&buf[0]))
	for _, priv := range tp.AllPrivileges() {
		if priv.Luid == luid {
			return priv.Attributes&windows.SE_PRIVILEGE_ENABLED != 0, nil
		}
	}
	return false, nil
}

func tokenHasEnabledPrivilege(t *testing.T, tok windows.Token, name string) bool {
	t.Helper()
	enabled, err := hasEnabledPrivilege(tok, name)
	if err != nil {
		t.Fatal(err)
	}
	return enabled
}

func TestWithPrivilegesRestoresAndIsolates(t *testing.T) {
	requireElevated(t)

	var procTok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &procTok); err != nil {
		t.Fatalf("OpenProcessToken: %v", err)
	}
	defer procTok.Close()
	beforeProc := make(map[string]bool, len(repairPrivileges))
	for _, name := range repairPrivileges {
		beforeProc[name] = tokenHasEnabledPrivilege(t, procTok, name)
	}

	assertNoThreadToken := func(ctx string) {
		t.Helper()
		var tok windows.Token
		err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &tok)
		if err == nil {
			tok.Close()
			t.Errorf("%s: thread unexpectedly has an impersonation token", ctx)
			return
		}
		if !errors.Is(err, windows.ERROR_NO_TOKEN) {
			t.Errorf("%s: OpenThreadToken = %v, want ERROR_NO_TOKEN", ctx, err)
		}
	}

	// Lock the outer test goroutine while checking before/after thread token
	// state on the same OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	assertNoThreadToken("before withPrivileges")

	checkOnce := func(returnErr error) {
		err := withPrivileges(func() error {
			var threadTok windows.Token
			if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &threadTok); err != nil {
				t.Errorf("OpenThreadToken inside withPrivileges: %v", err)
				return returnErr
			}
			defer threadTok.Close()
			for _, name := range repairPrivileges {
				if !tokenHasEnabledPrivilege(t, threadTok, name) {
					t.Errorf("thread token missing enabled %s inside withPrivileges", name)
				}
			}

			// A concurrent goroutine must not observe the impersonation token
			// or modified process-token privileges.
			done := make(chan error, 1)
			go func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				var otherTok windows.Token
				err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &otherTok)
				if err == nil {
					otherTok.Close()
					done <- errors.New("other goroutine observed a thread impersonation token")
					return
				}
				if !errors.Is(err, windows.ERROR_NO_TOKEN) {
					done <- fmt.Errorf("other goroutine OpenThreadToken: %w", err)
					return
				}
				for _, name := range repairPrivileges {
					enabled, err := hasEnabledPrivilege(procTok, name)
					if err != nil {
						done <- fmt.Errorf("checking process token privilege %s: %w", name, err)
						return
					}
					if enabled != beforeProc[name] {
						done <- fmt.Errorf("process token privilege %s changed while withPrivileges was active", name)
						return
					}
				}
				done <- nil
			}()
			if gerr := <-done; gerr != nil {
				t.Error(gerr)
			}
			return returnErr
		})
		if !errors.Is(err, returnErr) {
			t.Errorf("withPrivileges() = %v, want %v", err, returnErr)
		}
		assertNoThreadToken(fmt.Sprintf("after withPrivileges (err=%v)", returnErr))
		for _, name := range repairPrivileges {
			if tokenHasEnabledPrivilege(t, procTok, name) != beforeProc[name] {
				t.Errorf("process token privilege %s changed after withPrivileges", name)
			}
		}
	}

	checkOnce(nil)
	checkOnce(errors.New("callback error"))
}

func TestPrepareRootRefusesSharingViolation(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	setDescriptor(t, root, usersModifySDDL)
	_, before := descriptor(t, root)

	// Hold an open handle with DELETE access so opening without
	// FILE_SHARE_DELETE triggers ERROR_SHARING_VIOLATION.
	h, err := openNoFollow(root, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		t.Fatalf("opening competing DELETE handle on %q: %v", root, err)
	}
	defer windows.CloseHandle(h)

	origWrite := writeSecurity
	t.Cleanup(func() { writeSecurity = origWrite })
	writeCalls := 0
	writeSecurity = func(h windows.Handle, info windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
		writeCalls++
		return origWrite(h, info, owner, dacl)
	}

	if r, err := PrepareRoot(root, testSubdirs); err == nil {
		t.Fatalf("PrepareRoot(%q) = %+v, want sharing-violation error", root, r)
	} else if !strings.Contains(err.Error(), "open in another program that can delete or rename it") {
		t.Errorf("PrepareRoot(%q) error = %q, want clear sharing-violation guidance", root, err)
	}
	if writeCalls != 0 {
		t.Errorf("writeSecurity called %d times after sharing violation, want 0", writeCalls)
	}
	checkUnchanged(t, root, before)
}

func TestPrepareRootRefusesVolumeWithoutACLs(t *testing.T) {
	requireElevated(t)
	orig := volumeFlags
	t.Cleanup(func() { volumeFlags = orig })
	volumeFlags = func(string) (string, uint32, error) {
		return `E:\`, 0, nil
	}

	parent := filepath.Join(t.TempDir(), "nested")
	root := filepath.Join(parent, "googet")
	if r, err := PrepareRoot(root, testSubdirs); err == nil {
		t.Fatalf("PrepareRoot(%q) = %+v, want volume-permission error", root, r)
	} else if !strings.Contains(err.Error(), "doesn't support file permissions") {
		t.Errorf("PrepareRoot(%q) error = %q, want message about unsupported file permissions", root, err)
	}
	if _, err := os.Lstat(parent); !os.IsNotExist(err) {
		t.Errorf("PrepareRoot created parent %q on unsupported volume (Lstat error: %v)", parent, err)
	}
}

func TestPrepareRootInjectedFailures(t *testing.T) {
	requireElevated(t)

	t.Run("privilege setup error", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		orig := runWithPrivileges
		t.Cleanup(func() { runWithPrivileges = orig })
		runWithPrivileges = func(func() error) error {
			return errors.New("injected privilege error")
		}
		if r, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Fatalf("PrepareRoot(%q) = %+v, want error", root, r)
		} else if !strings.Contains(err.Error(), "injected privilege error") {
			t.Errorf("error = %q, want injected privilege error", err)
		}
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Errorf("PrepareRoot created %q after privilege failure (Lstat error: %v)", root, err)
		}
	})

	t.Run("SetSecurityInfo failure", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		mkdir(t, root)
		setDescriptor(t, root, usersModifySDDL)
		orig := writeSecurity
		t.Cleanup(func() { writeSecurity = orig })
		writeSecurity = func(windows.Handle, windows.SECURITY_INFORMATION, *windows.SID, *windows.ACL) error {
			return errors.New("injected write error")
		}
		if r, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Fatalf("PrepareRoot(%q) = %+v, want error", root, r)
		} else if !strings.Contains(err.Error(), "injected write error") || !strings.Contains(err.Error(), "remove the root and run googet again") {
			t.Errorf("error = %q, want write error with remediation guidance", err)
		}
	})

	t.Run("root read-back mismatch fails closed", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		mkdir(t, root)
		setDescriptor(t, root, usersModifySDDL)
		orig := writeSecurity
		t.Cleanup(func() { writeSecurity = orig })
		// Write a non-canonical DACL instead of the requested one so read-back
		// catches the discrepancy.
		writeSecurity = func(h windows.Handle, info windows.SECURITY_INFORMATION, owner *windows.SID, _ *windows.ACL) error {
			badSD, err := windows.SecurityDescriptorFromString(usersModifySDDL)
			if err != nil {
				return err
			}
			badDACL, _, err := badSD.DACL()
			if err != nil {
				return err
			}
			return orig(h, info, owner, badDACL)
		}
		if r, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Fatalf("PrepareRoot(%q) = %+v, want read-back error", root, r)
		} else if !strings.Contains(err.Error(), "still not the expected ones after the repair") {
			t.Errorf("error = %q, want read-back mismatch message", err)
		}
	})

	t.Run("subdirectory read-back mismatch fails closed", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		prepareRoot(t, root)
		repos := filepath.Join(root, "repos")
		mkdir(t, repos)
		setDescriptor(t, repos, "D:AI(A;OICI;0x1301bf;;;BU)")
		orig := writeSecurity
		t.Cleanup(func() { writeSecurity = orig })
		writeSecurity = func(windows.Handle, windows.SECURITY_INFORMATION, *windows.SID, *windows.ACL) error {
			return nil
		}
		if r, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Fatalf("PrepareRoot(%q) = %+v, want subdir read-back error", root, r)
		} else if !strings.Contains(err.Error(), "still not the expected ones after the repair") {
			t.Errorf("error = %q, want read-back mismatch message", err)
		}
	})
}

func TestPrepareRootKeepsLocalAdminMemberOwner(t *testing.T) {
	requireElevated(t)
	members, err := localAdministrators()
	if err != nil {
		t.Fatalf("localAdministrators(): %v", err)
	}
	if len(members) == 0 {
		t.Fatal("localAdministrators() returned no members")
	}
	var curUser string
	if u, err := windows.GetCurrentProcessToken().GetTokenUser(); err == nil {
		curUser = u.User.Sid.String()
	}
	// Prefer a local Administrators member other than the current process user
	// so the test exercises NetLocalGroupGetMembers specifically.
	var targetSID string
	for _, m := range members {
		if trustedSIDs[m] {
			continue
		}
		if m != curUser {
			targetSID = m
			break
		}
		if targetSID == "" {
			targetSID = m
		}
	}
	if targetSID == "" {
		t.Fatalf("no non-builtin local Administrators member found in %v", members)
	}

	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	setDescriptor(t, root, "O:"+targetSID+strings.TrimPrefix(rootSDDL, "O:BAG:SY"))
	if s, _ := descriptor(t, root); s.OwnerSID != targetSID {
		t.Fatalf("precondition: owner of %q = %s, want %s", root, s.OwnerSID, targetSID)
	}
	r := prepareRoot(t, root)
	if r.OwnerReset || r.DACLReplaced || r.Changed() {
		t.Errorf("PrepareRoot(%q) = %+v, want local Administrators member owner %s kept unchanged", root, r, targetSID)
	}
	checkCanonicalRoot(t, root, targetSID)
}

type fakeElevationTB struct {
	fatalMsg string
	skipMsg  string
}

func (*fakeElevationTB) Helper()             {}
func (f *fakeElevationTB) Fatal(args ...any) { f.fatalMsg = fmt.Sprint(args...) }
func (f *fakeElevationTB) Skip(args ...any)  { f.skipMsg = fmt.Sprint(args...) }

func TestCheckElevationGuard(t *testing.T) {
	for _, tc := range []struct {
		desc      string
		elevated  bool
		isCI      bool
		wantFatal bool
		wantSkip  bool
	}{
		{desc: "elevated locally", elevated: true, isCI: false},
		{desc: "elevated on CI", elevated: true, isCI: true},
		{desc: "unelevated locally skips", elevated: false, isCI: false, wantSkip: true},
		{desc: "unelevated on CI fails", elevated: false, isCI: true, wantFatal: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var tb fakeElevationTB
			checkElevation(&tb, tc.elevated, tc.isCI)
			if gotFatal := tb.fatalMsg != ""; gotFatal != tc.wantFatal {
				t.Errorf("fatal = %v (%q), want %v", gotFatal, tb.fatalMsg, tc.wantFatal)
			}
			if gotSkip := tb.skipMsg != ""; gotSkip != tc.wantSkip {
				t.Errorf("skip = %v (%q), want %v", gotSkip, tb.skipMsg, tc.wantSkip)
			}
		})
	}
}
