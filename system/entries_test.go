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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// testEntryRules is the policy that googet uses.
var testEntryRules = []EntryRule{
	{Pattern: "googet.lock", Action: EntryRemove},
	{Pattern: "googet.log", Action: EntryRemove},
	{Pattern: "googet.log.old*", Action: EntryRemove},
	{Pattern: "googet.conf", Action: EntryQuarantine},
	{Pattern: "googet.db", Action: EntryFail},
	{Pattern: "googet.db-journal", Action: EntryFail},
	{Pattern: "googet.db-wal", Action: EntryFail},
	{Pattern: "googet.db-shm", Action: EntryFail},
	{Pattern: "googet.state", Action: EntryFail},
	{Pattern: "googet.state.bak", Action: EntryFail},
	{Dir: "repos", Pattern: "*.repo", Action: EntryQuarantine},
	{Dir: "repos", Pattern: "*", Action: EntryRemoveLink},
	{Dir: "cache", Pattern: "*", Action: EntryRemove},
}

func TestValidateRules(t *testing.T) {
	if err := validateRules(testEntryRules); err != nil {
		t.Errorf("validateRules(testEntryRules) = %v, want nil", err)
	}
	for _, r := range []EntryRule{
		{Dir: ".", Pattern: "x", Action: EntryRemove},
		{Dir: "..", Pattern: "x", Action: EntryRemove},
		{Dir: `a\b`, Pattern: "x", Action: EntryRemove},
		{Dir: "a/b", Pattern: "x", Action: EntryRemove},
		{Dir: "C:", Pattern: "x", Action: EntryRemove},
		{Pattern: "", Action: EntryRemove},
		{Pattern: `..\x`, Action: EntryRemove},
		{Pattern: "a/b", Action: EntryRemove},
		{Pattern: "a:b", Action: EntryRemove},
		{Pattern: "[", Action: EntryRemove},
		{Pattern: "[a-", Action: EntryRemove},
		{Pattern: "x"},
		{Pattern: "x", Action: -1},
		{Pattern: "x", Action: EntryRemoveLink + 1},
	} {
		if err := validateRules([]EntryRule{r}); err == nil {
			t.Errorf("validateRules(%+v) = nil, want an error", r)
		}
	}
}

func TestRuleDirs(t *testing.T) {
	want := []string{"", "repos", "cache"}
	if diff := cmp.Diff(want, ruleDirs(testEntryRules)); diff != "" {
		t.Errorf("ruleDirs mismatch (-want +got):\n%s", diff)
	}
	mixed := []EntryRule{
		{Dir: "repos", Pattern: "*.repo", Action: EntryQuarantine},
		{Dir: "REPOS", Pattern: "*", Action: EntryRemoveLink},
		{Dir: "Cache", Pattern: "*", Action: EntryRemove},
	}
	if diff := cmp.Diff([]string{"repos", "Cache"}, ruleDirs(mixed)); diff != "" {
		t.Errorf("ruleDirs case-insensitive dedup mismatch (-want +got):\n%s", diff)
	}
}

func TestMatchRule(t *testing.T) {
	for _, tc := range []struct {
		dir, name string
		want      EntryAction
	}{
		{"", "googet.lock", EntryRemove},
		{"", "GooGet.Log", EntryRemove},
		{"", "googet.log.old", EntryRemove},
		{"", "googet.log.old.2", EntryRemove},
		{"", "googet.conf", EntryQuarantine},
		{"", "GOOGET.DB", EntryFail},
		{"", "googet.db-journal", EntryFail},
		{"", "googet.db-wal", EntryFail},
		{"", "googet.db-shm", EntryFail},
		{"", "googet.state", EntryFail},
		{"", "googet.exe", 0},
		{"", "googet.conf.untrusted-1", 0},
		{"", "x.repo", 0},
		{"REPOS", "x.repo", EntryQuarantine},
		{"repos", "x.REPO", EntryQuarantine},
		{"repos", "x.repo.untrusted-1", EntryRemoveLink},
		{"repos", "notes.txt", EntryRemoveLink},
		{"cache", "pkg.x86_64.1.0.goo", EntryRemove},
		{"other", "googet.conf", 0},
	} {
		r, ok := matchRule(testEntryRules, tc.dir, tc.name)
		if got := map[bool]EntryAction{true: r.Action, false: 0}[ok]; got != tc.want {
			t.Errorf("matchRule(%q, %q) = %v, %v; want action %v", tc.dir, tc.name, r, ok, tc.want)
		}
	}
}

func TestEntryReasons(t *testing.T) {
	inherited := func(mask uint32, sid string) aceInfo {
		return allow(aceFlagInherited, mask, sid)
	}
	canonicalFile := []aceInfo{inherited(accessFileAllAccess, sidAdministrators), inherited(accessFileAllAccess, sidSystem), inherited(readMask, sidUsers)}
	file := func(owner string, aces ...aceInfo) entryState {
		return entryState{ACL: aclState{OwnerSID: owner, ACEs: append(append([]aceInfo(nil), canonicalFile...), aces...)}, Links: 1}
	}
	dir := func(owner string, aces ...aceInfo) entryState {
		s := file(owner, aces...)
		s.IsDir = true
		return s
	}
	for _, tc := range []struct {
		desc string
		s    entryState
		want []string
	}{
		{desc: "inherited from the root, owned by Administrators", s: file(sidAdministrators)},
		{desc: "owned by SYSTEM", s: file(sidSystem)},
		{desc: "owned by a member of Administrators", s: file(sidTestAdmin)},
		{desc: "full control for a member of Administrators", s: file(sidAdministrators, allow(0, accessFileAllAccess, sidTestAdmin))},
		{desc: "read-only extra ACE", s: file(sidAdministrators, allow(0, readMask, sidEveryone))},
		{desc: "generic read and execute", s: file(sidAdministrators, allow(0, genericRead|accessGenericExecute, sidUsers))},
		{desc: "deny ACE", s: file(sidAdministrators, aceInfo{Type: aceTypeAccessDenied, Mask: accessFileAllAccess, SID: sidUsers})},
		{desc: "inherit-only write ACE on a file", s: file(sidAdministrators, allow(oici|aceFlagInheritOnly, maskModify, sidUsers))},
		{desc: "CREATOR OWNER on a directory", s: dir(sidAdministrators, allow(oici|aceFlagInheritOnly, accessGenericAll, sidCreatorOwner))},
		{desc: "OWNER RIGHTS with a trusted owner", s: file(sidAdministrators, allow(0, accessFileAllAccess, sidOwnerRights))},
		{
			desc: "untrusted owner",
			s:    file(sidTestUser),
			want: []string{"isn't owned by Administrators, SYSTEM or a member of Administrators (its owner is " + sidTestUser + ")"},
		},
		{
			desc: "no owner",
			s:    file(""),
			want: []string{"isn't owned by Administrators, SYSTEM or a member of Administrators (its owner is an unknown SID)"},
		},
		{
			desc: "explicit Users write ACE",
			s:    file(sidAdministrators, allow(0, maskModify, sidUsers)),
			want: []string{"lets " + sidUsers + " change it"},
		},
		{
			desc: "inherited Users write ACE",
			s:    file(sidAdministrators, inherited(maskModify, sidUsers)),
			want: []string{"lets " + sidUsers + " change it"},
		},
		{
			desc: "generic write",
			s:    file(sidAdministrators, allow(0, genericWrite, sidAuthenticatedUsers)),
			want: []string{"lets " + sidAuthenticatedUsers + " change it"},
		},
		{
			desc: "generic all for two SIDs",
			s:    file(sidAdministrators, allow(0, accessGenericAll, sidEveryone), allow(0, accessDelete, sidTestUser)),
			want: []string{"lets " + sidEveryone + ", " + sidTestUser + " change it"},
		},
		{
			desc: "inherit-only write ACE on a directory",
			s:    dir(sidAdministrators, allow(oici|aceFlagInheritOnly, maskModify, sidUsers)),
			want: []string{"lets " + sidUsers + " change it"},
		},
		{
			desc: "object ACE",
			s:    file(sidAdministrators, aceInfo{Type: aceTypeObject, Mask: maskModify}),
			want: []string{"lets an unknown SID change it"},
		},
		{
			desc: "OWNER RIGHTS with an untrusted owner",
			s:    file(sidTestUser, allow(0, accessWriteDAC, sidOwnerRights)),
			want: []string{
				"isn't owned by Administrators, SYSTEM or a member of Administrators (its owner is " + sidTestUser + ")",
				"lets " + sidOwnerRights + " change it",
			},
		},
		{
			desc: "NULL DACL",
			s:    entryState{ACL: aclState{OwnerSID: sidAdministrators, NullDACL: true}, Links: 1},
			want: []string{"has no access list, so everyone can change it"},
		},
		{
			desc: "reparse point",
			s:    entryState{ACL: aclState{OwnerSID: sidAdministrators, ACEs: canonicalFile}, IsLink: true, Links: 1},
			want: []string{"is a junction, symbolic link or other reparse point"},
		},
		{
			desc: "two hard links",
			s:    entryState{ACL: aclState{OwnerSID: sidAdministrators, ACEs: canonicalFile}, Links: 2},
			want: []string{"has 2 hard links"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, entryReasons(tc.s, testTrustedOwner)); diff != "" {
				t.Errorf("entryReasons mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestACLReasonsRejectsEveryNonReadBit(t *testing.T) {
	// Explicit literal for readMask (0x001200a9) | GENERIC_READ (0x80000000) |
	// GENERIC_EXECUTE (0x20000000), independent of production constants.
	const allowed uint32 = 0xa01200a9
	for bit := uint(0); bit < 32; bit++ {
		mask := uint32(1) << bit
		s := aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(0, mask, sidUsers)}}
		got := aclReasons(s, false, true, testTrustedOwner)
		wantUntrusted := mask&^allowed != 0
		if (len(got) > 0) != wantUntrusted {
			t.Errorf("bit %d (%#x): aclReasons = %q, want untrusted=%v", bit, mask, got, wantUntrusted)
		}
	}
}

func TestACLReasonsEdgeCases(t *testing.T) {
	const (
		sidCreatorOwnerServer = "S-1-3-2"
		sidCreatorGroupServer = "S-1-3-3"
	)
	for _, tc := range []struct {
		desc         string
		s            aclState
		isDir        bool
		ownerTrusted bool
		want         []string
	}{
		{
			desc:         "empty DACL is trusted",
			s:            aclState{OwnerSID: sidAdministrators},
			ownerTrusted: true,
		},
		{
			desc:         "CREATOR GROUP inherit-only on a directory is ignored",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(oici|aceFlagInheritOnly, accessGenericAll, sidCreatorGroup)}},
			isDir:        true,
			ownerTrusted: true,
		},
		{
			desc:         "CREATOR OWNER without inherit-only on a file is flagged",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(0, accessGenericAll, sidCreatorOwner)}},
			ownerTrusted: true,
			want:         []string{"lets " + sidCreatorOwner + " change it"},
		},
		{
			desc:         "CREATOR OWNER without inherit-only on a directory is flagged",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(oici, accessGenericAll, sidCreatorOwner)}},
			isDir:        true,
			ownerTrusted: true,
			want:         []string{"lets " + sidCreatorOwner + " change it"},
		},
		{
			desc:         "CREATOR GROUP without inherit-only on a directory is flagged",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(oici, maskModify, sidCreatorGroup)}},
			isDir:        true,
			ownerTrusted: true,
			want:         []string{"lets " + sidCreatorGroup + " change it"},
		},
		{
			desc:         "other creator authority SIDs are flagged even when inherit-only on a directory",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(oici|aceFlagInheritOnly, maskModify, sidCreatorOwnerServer), allow(0, accessDelete, sidCreatorGroupServer)}},
			isDir:        true,
			ownerTrusted: true,
			want:         []string{"lets " + sidCreatorOwnerServer + ", " + sidCreatorGroupServer + " change it"},
		},
		{
			desc:         "OWNER RIGHTS read-only with an untrusted owner is ignored by aclReasons",
			s:            aclState{OwnerSID: sidTestUser, ACEs: []aceInfo{allow(0, readMask, sidOwnerRights)}},
			ownerTrusted: false,
		},
		{
			desc:         "OWNER RIGHTS write on a directory with a trusted owner is ignored",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(oici, accessFileAllAccess, sidOwnerRights)}},
			isDir:        true,
			ownerTrusted: true,
		},
		{
			desc: "all deny ACE types are ignored",
			s: aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{
				{Type: aceTypeAccessDenied, Mask: accessFileAllAccess, SID: sidUsers},
				{Type: aceTypeAccessDeniedCallback, Mask: accessFileAllAccess, SID: sidUsers},
				{Type: aceTypeAccessDeniedObject, Mask: accessFileAllAccess},
				{Type: aceTypeAccessDeniedCallbackObject, Mask: accessFileAllAccess},
			}},
			ownerTrusted: true,
		},
		{
			desc:         "callback allow ACE for an untrusted SID is flagged",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{{Type: aceTypeAccessAllowedCallback, Mask: maskModify, SID: sidUsers}}},
			ownerTrusted: true,
			want:         []string{"lets " + sidUsers + " change it"},
		},
		{
			desc:         "callback allow ACE for a trusted SID is ignored",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{{Type: aceTypeAccessAllowedCallback, Mask: accessFileAllAccess, SID: sidTestAdmin}}},
			ownerTrusted: true,
		},
		{
			desc:         "callback object allow ACE with write mask is flagged as unknown SID",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{{Type: aceTypeAccessAllowedCallbackObject, Mask: accessFileWriteData}}},
			ownerTrusted: true,
			want:         []string{"lets an unknown SID change it"},
		},
		{
			desc:         "object and callback object allow ACEs with read-only mask are ignored",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{{Type: aceTypeAccessAllowedObject, Mask: readMask}, {Type: aceTypeAccessAllowedCallbackObject, Mask: readMask}}},
			ownerTrusted: true,
		},
		{
			desc:         "duplicate untrusted SID is reported once",
			s:            aclState{OwnerSID: sidAdministrators, ACEs: []aceInfo{allow(0, accessFileWriteData, sidUsers), allow(aceFlagInherited, accessDelete, sidUsers)}},
			ownerTrusted: true,
			want:         []string{"lets " + sidUsers + " change it"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, aclReasons(tc.s, tc.isDir, tc.ownerTrusted, testTrustedOwner)); diff != "" {
				t.Errorf("aclReasons mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// Verify entryReasons reports every failing property together in order.
	combined := entryState{
		ACL:    aclState{OwnerSID: sidTestUser, NullDACL: true},
		IsLink: true,
		Links:  3,
	}
	wantCombined := []string{
		"is a junction, symbolic link or other reparse point",
		"has 3 hard links",
		"isn't owned by Administrators, SYSTEM or a member of Administrators (its owner is " + sidTestUser + ")",
		"has no access list, so everyone can change it",
	}
	if diff := cmp.Diff(wantCombined, entryReasons(combined, testTrustedOwner)); diff != "" {
		t.Errorf("combined entryReasons mismatch (-want +got):\n%s", diff)
	}
}

func TestPlanEntry(t *testing.T) {
	for _, tc := range []struct {
		action    EntryAction
		isLink    bool
		untrusted bool
		wantOp    entryOp
		wantFail  bool
	}{
		{EntryFail, false, false, opKeep, false},
		{EntryQuarantine, true, false, opKeep, false},
		{EntryRemove, false, false, opKeep, false},
		{EntryRemoveLink, true, false, opKeep, false},
		{EntryFail, false, true, opKeep, true},
		{EntryFail, true, true, opKeep, true},
		{EntryQuarantine, false, true, opQuarantine, true},
		{EntryQuarantine, true, true, opRemove, true},
		{EntryRemove, false, true, opRemove, false},
		{EntryRemove, true, true, opRemove, false},
		{EntryRemoveLink, false, true, opKeep, false},
		{EntryRemoveLink, true, true, opRemove, false},
		{0, false, true, opKeep, true},
	} {
		op, fail := planEntry(tc.action, tc.isLink, tc.untrusted)
		if op != tc.wantOp || fail != tc.wantFail {
			t.Errorf("planEntry(%v, link %v, untrusted %v) = %v, %v; want %v, %v", tc.action, tc.isLink, tc.untrusted, op, fail, tc.wantOp, tc.wantFail)
		}
	}
}

func TestQuarantinePathIsIgnored(t *testing.T) {
	ts := time.Unix(0, 1234)
	for _, path := range []string{filepath.Join("root", "repos", "x.repo"), filepath.Join("root", "googet.conf")} {
		got := quarantinePath(path, ts)
		if want := path + ".untrusted-1234"; got != want {
			t.Errorf("quarantinePath(%q) = %q, want %q", path, got, want)
		}
		if filepath.Dir(got) != filepath.Dir(path) {
			t.Errorf("quarantinePath(%q) = %q, want it in the same directory", path, got)
		}
		dir := filepath.Base(filepath.Dir(path))
		if dir == "root" {
			dir = ""
		}
		if r, ok := matchRule(testEntryRules, dir, filepath.Base(got)); ok && r.Action != EntryRemoveLink {
			t.Errorf("quarantinePath(%q) = %q matches %+v, want no rule that acts on files", path, got, r)
		}
		if ok, _ := filepath.Match("*.repo", filepath.Base(got)); ok {
			t.Errorf("quarantinePath(%q) = %q, want a name that doesn't match *.repo", path, got)
		}
	}
}

func TestFailureMessage(t *testing.T) {
	permReasons := []string{"lets S-1-5-32-545 change it"}
	hardLinkReasons := []string{"has 2 hard links", "lets S-1-5-32-545 change it"}
	linkReasons := []string{"is a junction, symbolic link or other reparse point"}
	for _, tc := range []struct {
		desc    string
		reasons []string
		op      entryOp
		newPath string
		state   entryState
		want    []string
		forbid  []string
	}{
		{
			desc:    "keep permissions",
			reasons: permReasons,
			op:      opKeep,
			want:    []string{`C:\g\googet.db lets S-1-5-32-545 change it;`, "owner Administrators", "run googet again"},
		},
		{
			desc:    "keep hard links",
			reasons: hardLinkReasons,
			op:      opKeep,
			state:   entryState{Links: 2},
			want:    []string{`C:\g\googet.db has 2 hard links, lets S-1-5-32-545 change it;`, "extra hard links", "run googet again"},
		},
		{
			desc:    "keep link",
			reasons: linkReasons,
			op:      opKeep,
			state:   entryState{IsLink: true},
			want:    []string{`C:\g\googet.db is a junction`, "replace the link with a regular file", "run googet again"},
		},
		{
			desc:    "quarantine permissions",
			reasons: permReasons,
			op:      opQuarantine,
			newPath: `C:\g\googet.conf.untrusted-1`,
			want:    []string{"renamed it to " + `C:\g\googet.conf.untrusted-1`, "rename it back", "run googet again"},
		},
		{
			desc:    "quarantine hard links",
			reasons: hardLinkReasons,
			op:      opQuarantine,
			newPath: `C:\g\googet.conf.untrusted-1`,
			state:   entryState{Links: 2},
			want:    []string{"renamed it to " + `C:\g\googet.conf.untrusted-1`, "copy its contents to a new file", "extra hard links", "run googet again"},
			forbid:  []string{"rename it back"},
		},
		{
			desc:    "remove",
			reasons: hardLinkReasons,
			op:      opRemove,
			state:   entryState{Links: 2},
			want:    []string{"removed it", "run googet again"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got := failureMessage(`C:\g\googet.db`, tc.reasons, tc.op, tc.newPath, tc.state)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("failureMessage = %q, want it to contain %q", got, w)
				}
			}
			for _, f := range tc.forbid {
				if strings.Contains(got, f) {
					t.Errorf("failureMessage = %q, must not contain %q", got, f)
				}
			}
		})
	}
}

func TestCombineReasons(t *testing.T) {
	got := combineReasons([]string{"a", "b"}, []string{"b", "c"})
	want := []string{"a", "b", "c (before googet reset the permissions of the googet root)"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("combineReasons mismatch (-want +got):\n%s", diff)
	}
	if got := combineReasons(nil, nil); len(got) != 0 {
		t.Errorf("combineReasons(nil, nil) = %q, want none", got)
	}
}

func TestRecordBefore(t *testing.T) {
	var r RootReport
	r.recordBeforeKey(entryPathKey(`C:\G\Googet.conf`), nil)
	if r.untrustedBefore != nil {
		t.Errorf("recordBeforeKey with no reasons recorded %v", r.untrustedBefore)
	}
	r.recordBeforeKey(entryPathKey(filepath.Join("G", "Googet.conf")), []string{"x"})
	if got := r.beforeReasons("", filepath.Join("g", "googet.conf")); !cmp.Equal(got, []string{"x"}) {
		t.Errorf("beforeReasons by path (different case) = %q, want [x]", got)
	}
	r.recordBeforeKey("id128:0000000000000001:00000000000000000000000000000002", []string{"y"})
	if got := r.beforeReasons("id128:0000000000000001:00000000000000000000000000000002", filepath.Join("g", "other.conf")); !cmp.Equal(got, []string{"y"}) {
		t.Errorf("beforeReasons by file ID = %q, want [y]", got)
	}
	if got := r.beforeReasons("id128:0000000000000001:00000000000000000000000000000002", filepath.Join("g", "googet.conf")); !cmp.Equal(got, []string{"y", "x"}) {
		t.Errorf("beforeReasons merged = %q, want [y x]", got)
	}
}

func TestEntryReport(t *testing.T) {
	a := EntryReport{Removed: []string{"l"}, Failures: []string{"f1"}}
	b := EntryReport{Quarantined: map[string]string{"r2": "r2.u", "r1": "r1.u"}, Failures: []string{"f2"}}
	if a.Changed() != true || (EntryReport{Failures: []string{"f"}}).Changed() || (EntryReport{}).Failed() {
		t.Errorf("Changed or Failed is wrong")
	}
	m := a.Merge(b)
	want := EntryReport{Removed: []string{"l"}, Quarantined: map[string]string{"r1": "r1.u", "r2": "r2.u"}, Failures: []string{"f1", "f2"}}
	if diff := cmp.Diff(want, m); diff != "" {
		t.Errorf("Merge mismatch (-want +got):\n%s", diff)
	}
	if got, want := m.Summary(), `removed ["l"]; renamed "r1" to "r1.u"; renamed "r2" to "r2.u"`; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	if m := (EntryReport{}).Merge(EntryReport{}); m.Changed() || m.Failed() || m.Quarantined != nil {
		t.Errorf("merging empty reports = %+v, want an empty report", m)
	}
}
