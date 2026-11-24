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
	"testing"

	"github.com/google/go-cmp/cmp"
)

const (
	sidEveryone           = "S-1-1-0"
	sidAuthenticatedUsers = "S-1-5-11"
	sidInteractive        = "S-1-5-4"
	sidTestUser           = "S-1-5-21-1-2-3-1001"
	sidTestAdmin          = "S-1-5-21-1-2-3-500"

	oici                 = aceFlagObjectInherit | aceFlagContainerInherit
	maskModify           = 0x1301bf
	accessFileWriteData  = 0x2
	accessFileAppendData = 0x4
	accessFileWriteEA    = 0x10
	accessFileWriteAttrs = 0x100
	genericWrite         = 0x40000000
	genericRead          = 0x80000000
	aceTypeObject        = aceTypeAccessAllowedObject
)

func testTrustedOwner(sid string) bool {
	return trustedSIDs[sid] || sid == sidTestAdmin
}

func allow(flags uint8, mask uint32, sid string) aceInfo {
	return aceInfo{Type: aceTypeAccessAllowed, Flags: flags, Mask: mask, SID: sid}
}

func withACEs(extra ...aceInfo) []aceInfo {
	return append(append([]aceInfo(nil), canonicalACEs...), extra...)
}

func TestClassifyRoot(t *testing.T) {
	canonical := aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs()}
	for _, tc := range []struct {
		desc string
		s    aclState
		want rootPlan
	}{
		{
			desc: "canonical",
			s:    canonical,
		},
		{
			desc: "canonical in another order owned by a local administrator",
			s:    aclState{OwnerSID: sidTestAdmin, Protected: true, ACEs: []aceInfo{canonicalACEs[2], canonicalACEs[1], canonicalACEs[0]}},
		},
		{
			desc: "trusted SID with any access",
			s:    aclState{OwnerSID: sidSystem, Protected: true, ACEs: withACEs(allow(0, genericWrite, sidTrustedInstaller))},
		},
		{
			desc: "TrustedInstaller owner",
			s:    aclState{OwnerSID: sidTrustedInstaller, Protected: true, ACEs: withACEs()},
		},
		{
			desc: "Users owner",
			s:    aclState{OwnerSID: sidUsers, Protected: true, ACEs: withACEs()},
			want: rootPlan{ResetOwner: true},
		},
		{
			desc: "extra read-only ACE",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, readMask, sidEveryone))},
		},
		{
			desc: "untrusted owner",
			s:    aclState{OwnerSID: sidTestUser, Protected: true, ACEs: withACEs()},
			want: rootPlan{ResetOwner: true},
		},
		{
			desc: "no owner",
			s:    aclState{Protected: true, ACEs: withACEs()},
			want: rootPlan{ResetOwner: true},
		},
		{
			desc: "NULL DACL",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, NullDACL: true},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "unprotected",
			s:    aclState{OwnerSID: sidAdministrators, ACEs: withACEs()},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "missing canonical ACE",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: canonicalACEs[:2]},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "non-inheritable temporary lock ACEs",
			s: aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: []aceInfo{
				allow(0, accessFileAllAccess, sidAdministrators),
				allow(0, accessFileAllAccess, sidSystem),
				allow(0, readMask, sidUsers),
			}},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "non-inheritable temporary lock ACEs with custom read-only ACE",
			s: aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: []aceInfo{
				allow(0, accessFileAllAccess, sidAdministrators),
				allow(0, accessFileAllAccess, sidSystem),
				allow(0, readMask, sidUsers),
				allow(oici, readMask, sidEveryone),
			}},
			want: rootPlan{ReplaceDACL: true, DACL: withACEs(allow(oici, readMask, sidEveryone))},
		},
		{
			desc: "empty DACL",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "Users modify",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, maskModify, sidUsers))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "generic write",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, genericWrite, sidUsers))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "generic read",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, genericRead, sidUsers))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "inherit-only grant",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici|aceFlagInheritOnly, accessFileAllAccess, sidUsers))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "Everyone write",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, maskModify, sidEveryone))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "Authenticated Users write",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, maskModify, sidAuthenticatedUsers))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "CREATOR OWNER full control",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici|aceFlagInheritOnly, accessGenericAll, sidCreatorOwner))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "OWNER RIGHTS write DAC",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(0, accessWriteDAC, sidOwnerRights))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "OWNER RIGHTS read-only is stripped",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, readMask, sidOwnerRights))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "CREATOR OWNER read-only is stripped",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici|aceFlagInheritOnly, readMask, sidCreatorOwner))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "local administrator with full control",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, accessFileAllAccess, sidTestAdmin))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "deny ACE",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(aceInfo{Type: aceTypeAccessDenied, Mask: readMask, SID: sidUsers})},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "deny ACE for Administrators",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(aceInfo{Type: aceTypeAccessDenied, Mask: accessFileAllAccess, SID: sidAdministrators})},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "FILE_WRITE_DATA to Users",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, accessFileWriteData, sidUsers))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "FILE_APPEND_DATA to Authenticated Users",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, accessFileAppendData, sidAuthenticatedUsers))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "FILE_WRITE_EA to Everyone",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, accessFileWriteEA, sidEveryone))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "FILE_DELETE_CHILD to INTERACTIVE",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, accessFileDeleteChild, sidInteractive))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "FILE_WRITE_ATTRIBUTES to test user",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, accessFileWriteAttrs, sidTestUser))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "DELETE to Users",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, accessDelete, sidUsers))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "WRITE_OWNER to INTERACTIVE",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(0, accessWriteOwner, sidInteractive))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "GENERIC_ALL to Everyone",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, accessGenericAll, sidEveryone))},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "object ACE",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(aceInfo{Type: aceTypeObject, Flags: oici, Mask: readMask})},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "callback ACE",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(aceInfo{Type: aceTypeAccessAllowedCallback, Flags: oici, Mask: readMask, SID: sidUsers})},
			want: rootPlan{ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "inherited ACE in a protected DACL",
			s:    aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici|aceFlagInherited, readMask, sidEveryone))},
			want: rootPlan{ReplaceDACL: true, DACL: withACEs(allow(oici, readMask, sidEveryone))},
		},
		{
			desc: "inherited from C:\\ProgramData",
			s: aclState{OwnerSID: sidTestUser, ACEs: []aceInfo{
				allow(oici|aceFlagInherited, accessFileAllAccess, sidSystem),
				allow(oici|aceFlagInherited, accessFileAllAccess, sidAdministrators),
				allow(oici|aceFlagInheritOnly|aceFlagInherited, accessGenericAll, sidCreatorOwner),
				allow(oici|aceFlagInherited, readMask, sidUsers),
				allow(aceFlagContainerInherit|aceFlagInherited, 0x116, sidUsers),
			}},
			want: rootPlan{ResetOwner: true, ReplaceDACL: true, DACL: canonicalACEs},
		},
		{
			desc: "read-only extras kept and everything else reset",
			s: aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{
				allow(0, maskModify, sidUsers),
				allow(oici, readMask, sidEveryone),
				allow(aceFlagContainerInherit|aceFlagInherited, 0x1200a0, sidTestUser),
				allow(aceFlagContainerInherit, 0x1200a0, sidTestUser),
				allow(aceFlagNoPropagateInherit, 0x100000, sidAuthenticatedUsers),
				allow(aceFlagInheritOnly, readMask, sidAuthenticatedUsers),
				allow(0, 0, sidAuthenticatedUsers),
				allow(oici, accessFileAllAccess, sidSystem),
				{Type: aceTypeAccessDenied, Mask: readMask, SID: sidEveryone},
			}},
			want: rootPlan{ReplaceDACL: true, DACL: withACEs(
				allow(oici, readMask, sidEveryone),
				allow(aceFlagContainerInherit, 0x1200a0, sidTestUser),
				allow(0, 0x100000, sidAuthenticatedUsers),
			)},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got := classifyRoot(tc.s, testTrustedOwner)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("classifyRoot() mismatch (-want +got):\n%s", diff)
			}
			if !got.ReplaceDACL {
				return
			}
			// A repaired descriptor must be trusted.
			repaired := aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: got.DACL}
			if again := classifyRoot(repaired, testTrustedOwner); again.needed() {
				t.Errorf("classifyRoot(repaired) = %+v, want nothing to do", again)
			}
		})
	}
}

func TestClassifyRootRejectsEveryNonReadBit(t *testing.T) {
	for bit := uint(0); bit < 32; bit++ {
		mask := uint32(1) << bit
		s := aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: withACEs(allow(oici, mask, sidUsers))}
		got := classifyRoot(s, testTrustedOwner)
		wantReplace := mask&^readMask != 0
		if got.ReplaceDACL != wantReplace {
			t.Errorf("bit %d (%#x): ReplaceDACL = %v, want %v", bit, mask, got.ReplaceDACL, wantReplace)
		}
	}
}

func TestSubdirNeedsReset(t *testing.T) {
	inherited := func(aces []aceInfo) []aceInfo {
		var out []aceInfo
		for _, a := range aces {
			a.Flags |= aceFlagInherited
			out = append(out, a)
		}
		return out
	}
	for _, tc := range []struct {
		desc string
		s    aclState
		want bool
	}{
		{desc: "inherits the root", s: aclState{OwnerSID: sidAdministrators, ACEs: inherited(canonicalACEs)}},
		{desc: "owned by a local administrator", s: aclState{OwnerSID: sidTestAdmin, ACEs: inherited(canonicalACEs)}},
		{desc: "protected with trusted ACEs", s: aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: canonicalACEs}},
		{desc: "protected with non-inheritable temporary lock ACEs", s: aclState{OwnerSID: sidAdministrators, Protected: true, ACEs: []aceInfo{
			allow(0, accessFileAllAccess, sidAdministrators),
			allow(0, accessFileAllAccess, sidSystem),
			allow(0, readMask, sidUsers),
		}}, want: true},
		{desc: "untrusted owner", s: aclState{OwnerSID: sidTestUser, ACEs: inherited(canonicalACEs)}, want: true},
		{desc: "NULL DACL", s: aclState{OwnerSID: sidAdministrators, NullDACL: true}, want: true},
		{desc: "explicit Users write", s: aclState{OwnerSID: sidAdministrators, ACEs: append([]aceInfo{allow(oici, maskModify, sidUsers)}, inherited(canonicalACEs)...)}, want: true},
		{desc: "inherited Users write", s: aclState{OwnerSID: sidAdministrators, ACEs: inherited(withACEs(allow(aceFlagContainerInherit, 0x116, sidUsers)))}, want: true},
		{desc: "deny ACE", s: aclState{OwnerSID: sidAdministrators, ACEs: append([]aceInfo{{Type: aceTypeAccessDenied, Mask: readMask, SID: sidUsers}}, inherited(canonicalACEs)...)}, want: true},
		{desc: "OWNER RIGHTS read-only", s: aclState{OwnerSID: sidAdministrators, ACEs: append([]aceInfo{allow(oici, readMask, sidOwnerRights)}, inherited(canonicalACEs)...)}, want: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if got := subdirNeedsReset(tc.s, testTrustedOwner); got != tc.want {
				t.Errorf("subdirNeedsReset() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInheritsCanonical(t *testing.T) {
	var aces []aceInfo
	for _, a := range canonicalACEs {
		a.Flags |= aceFlagInherited
		aces = append(aces, a)
	}
	if !inheritsCanonical(aclState{OwnerSID: sidAdministrators, ACEs: aces}) {
		t.Errorf("inheritsCanonical(inherited canonical ACEs) = false, want true")
	}
	for _, s := range []aclState{
		{OwnerSID: sidAdministrators},
		{OwnerSID: sidAdministrators, ACEs: canonicalACEs},
		{OwnerSID: sidAdministrators, Protected: true, ACEs: aces},
		{OwnerSID: sidAdministrators, NullDACL: true},
	} {
		if inheritsCanonical(s) {
			t.Errorf("inheritsCanonical(%+v) = true, want false", s)
		}
	}
}

func TestAncestorRisks(t *testing.T) {
	for _, tc := range []struct {
		desc string
		s    aclState
		want []string
	}{
		{
			desc: "default volume root",
			s: aclState{OwnerSID: sidTrustedInstaller, ACEs: []aceInfo{
				allow(oici, accessFileAllAccess, sidAdministrators),
				allow(oici, accessFileAllAccess, sidSystem),
				allow(oici, readMask, sidUsers),
				allow(oici|aceFlagInheritOnly, maskModify, sidAuthenticatedUsers),
				allow(0, 0x4, sidAuthenticatedUsers),
			}},
		},
		{
			desc: "default ProgramData",
			s: aclState{OwnerSID: sidSystem, Protected: true, ACEs: []aceInfo{
				allow(oici, accessFileAllAccess, sidSystem),
				allow(oici, accessFileAllAccess, sidAdministrators),
				allow(oici|aceFlagInheritOnly, accessGenericAll, sidCreatorOwner),
				allow(oici, readMask, sidUsers),
				allow(aceFlagContainerInherit, 0x116, sidUsers),
			}},
		},
		{
			desc: "local administrator owner with full control",
			s:    aclState{OwnerSID: sidTestAdmin, ACEs: []aceInfo{allow(oici, accessFileAllAccess, sidTestAdmin)}},
		},
		{
			desc: "OWNER RIGHTS with a trusted owner",
			s:    aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(0, accessWriteDAC, sidOwnerRights)}},
		},
		{
			desc: "deny ACE",
			s:    aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{{Type: aceTypeAccessDenied, Mask: accessDelete, SID: sidUsers}}},
		},
		{
			desc: "untrusted owner",
			s:    aclState{OwnerSID: sidTestUser, ACEs: []aceInfo{allow(oici, accessWriteDAC, sidOwnerRights)}},
			want: []string{"owner " + sidTestUser, sidOwnerRights},
		},
		{
			desc: "NULL DACL",
			s:    aclState{OwnerSID: sidAdministrators, NullDACL: true},
			want: []string{"everyone (no access list)"},
		},
		{
			desc: "rights that replace the directory",
			s: aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{
				allow(0, accessDelete, sidUsers),
				allow(0, accessFileDeleteChild, sidEveryone),
				allow(0, accessWriteOwner, sidAuthenticatedUsers),
				allow(0, accessGenericAll, sidTestUser),
				allow(0, accessWriteDAC, sidUsers),
				{Type: aceTypeObject, Mask: accessDelete},
			}},
			want: []string{sidUsers, sidEveryone, sidAuthenticatedUsers, sidTestUser, "an unknown SID"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got := ancestorRisks(tc.s, testTrustedOwner)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ancestorRisks() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDACLSDDL(t *testing.T) {
	got, err := daclSDDL(withACEs(allow(aceFlagContainerInherit|aceFlagNoPropagateInherit|aceFlagInheritOnly, 0x1200a0, sidEveryone)))
	if err != nil {
		t.Fatalf("daclSDDL() failed: %v", err)
	}
	want := "D:PAI(A;OICI;0x1f01ff;;;S-1-5-32-544)(A;OICI;0x1f01ff;;;S-1-5-18)(A;OICI;0x1200a9;;;S-1-5-32-545)(A;CINPIO;0x1200a0;;;S-1-1-0)"
	if got != want {
		t.Errorf("daclSDDL() = %q, want %q", got, want)
	}
	for _, a := range []aceInfo{
		{Type: aceTypeAccessDenied, Mask: readMask, SID: sidUsers},
		allow(0, readMask, ""),
		allow(0, readMask, "S-1-5-32-545)(A;;FA;;;WD"),
		allow(0, readMask, "BU"),
		allow(aceFlagInherited, readMask, sidUsers),
	} {
		if s, err := daclSDDL([]aceInfo{a}); err == nil {
			t.Errorf("daclSDDL(%+v) = %q, want an error", a, s)
		}
	}
	const wantTempLock = "D:P(A;;FA;;;BA)(A;;FA;;;SY)(A;;0x1200a9;;;BU)"
	if got, err := tempLockDACL(canonicalACEs); err != nil || got != wantTempLock {
		t.Errorf("tempLockDACL(canonicalACEs) = %q, %v; want %q, nil", got, err, wantTempLock)
	}
	const wantTempLockWithEveryone = wantTempLock + "(A;OICI;0x1200a9;;;S-1-1-0)"
	if got, err := tempLockDACL(withACEs(allow(oici, readMask, sidEveryone))); err != nil || got != wantTempLockWithEveryone {
		t.Errorf("tempLockDACL(with Everyone) = %q, %v; want %q, nil", got, err, wantTempLockWithEveryone)
	}
}

func TestRootReportChanged(t *testing.T) {
	for _, tc := range []struct {
		r           RootReport
		want        bool
		wantSummary string
	}{
		{r: RootReport{}, want: false},
		{r: RootReport{Created: true, Warnings: []string{"w"}}, want: false},
		{r: RootReport{OwnerReset: true}, want: true, wantSummary: "set the owner to Administrators"},
		{r: RootReport{DACLReplaced: true, RemovedLinks: []string{`C:\r\repos`}}, want: true, wantSummary: `replaced the access list; removed links ["C:\\r\\repos"]`},
		{r: RootReport{RepairedSubdirs: []string{`C:\r\cache`}}, want: true, wantSummary: `reset the permissions of ["C:\\r\\cache"]`},
	} {
		if got := tc.r.Changed(); got != tc.want {
			t.Errorf("%+v.Changed() = %v, want %v", tc.r, got, tc.want)
		}
		if got := tc.r.Summary(); got != tc.wantSummary {
			t.Errorf("%+v.Summary() = %q, want %q", tc.r, got, tc.wantSummary)
		}
	}
}
