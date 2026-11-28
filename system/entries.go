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
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// EntryAction says what CheckEntries does with an entry that isn't trusted.
type EntryAction int

const (
	// EntryFail leaves the entry untouched and fails the run.
	EntryFail EntryAction = iota + 1
	// EntryQuarantine renames the entry, in the same directory, to a name
	// that googet doesn't read, and fails the run. A link is removed
	// instead.
	EntryQuarantine
	// EntryRemove deletes the entry: a file, a link but never its target, or
	// a directory tree, without following links in it.
	EntryRemove
	// EntryRemoveLink removes the entry if it is a link and keeps it
	// otherwise.
	EntryRemoveLink
)

// EntryRule applies Action to the direct children of Dir, a directory in the
// googet root or the root itself if empty, whose names match Pattern. Names
// are matched case-insensitively with filepath.Match; the first rule that
// matches an entry applies, and entries that no rule matches are ignored.
type EntryRule struct {
	Dir     string
	Pattern string
	Action  EntryAction
}

// EntryReport describes what CheckEntries found and did.
type EntryReport struct {
	// Removed lists the entries that were deleted.
	Removed []string
	// Quarantined maps each entry that was renamed to its new path.
	Quarantined map[string]string
	// Failures describes each entry that makes the run fail, and how to fix
	// it.
	Failures []string
}

// Changed reports whether CheckEntries removed or renamed an entry.
func (r EntryReport) Changed() bool {
	return len(r.Removed) > 0 || len(r.Quarantined) > 0
}

// Failed reports whether the run must fail.
func (r EntryReport) Failed() bool {
	return len(r.Failures) > 0
}

// Summary describes the changes that Changed reports.
func (r EntryReport) Summary() string {
	var parts []string
	if len(r.Removed) > 0 {
		parts = append(parts, fmt.Sprintf("removed %q", r.Removed))
	}
	for _, from := range sortedKeys(r.Quarantined) {
		parts = append(parts, fmt.Sprintf("renamed %q to %q", from, r.Quarantined[from]))
	}
	return strings.Join(parts, "; ")
}

// Merge returns the combination of r and o.
func (r EntryReport) Merge(o EntryReport) EntryReport {
	m := EntryReport{
		Removed:  append(append([]string(nil), r.Removed...), o.Removed...),
		Failures: append(append([]string(nil), r.Failures...), o.Failures...),
	}
	for _, q := range []map[string]string{r.Quarantined, o.Quarantined} {
		for from, to := range q {
			if m.Quarantined == nil {
				m.Quarantined = make(map[string]string)
			}
			m.Quarantined[from] = to
		}
	}
	return m
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateRules checks that each rule names the root or a direct child of it,
// has a valid pattern and a known action.
func validateRules(rules []EntryRule) error {
	for _, r := range rules {
		if r.Dir != "" && (r.Dir == "." || r.Dir == ".." || strings.ContainsAny(r.Dir, `\/:`)) {
			return fmt.Errorf("invalid googet root subdirectory %q", r.Dir)
		}
		if r.Pattern == "" || strings.ContainsAny(r.Pattern, `\/:`) {
			return fmt.Errorf("invalid entry pattern %q", r.Pattern)
		}
		if _, err := filepath.Match(r.Pattern, ""); err != nil {
			return fmt.Errorf("invalid entry pattern %q: %w", r.Pattern, err)
		}
		if r.Action < EntryFail || r.Action > EntryRemoveLink {
			return fmt.Errorf("invalid action %d for entry pattern %q", r.Action, r.Pattern)
		}
	}
	return nil
}

// ruleDirs returns the directories that rules name, in the order of their
// first rule.
func ruleDirs(rules []EntryRule) []string {
	var dirs []string
	for _, r := range rules {
		found := false
		for _, d := range dirs {
			if strings.EqualFold(d, r.Dir) {
				found = true
				break
			}
		}
		if !found {
			dirs = append(dirs, r.Dir)
		}
	}
	return dirs
}

// matchRule returns the first rule for dir whose pattern matches name.
func matchRule(rules []EntryRule, dir, name string) (EntryRule, bool) {
	for _, r := range rules {
		if !strings.EqualFold(r.Dir, dir) {
			continue
		}
		if ok, err := filepath.Match(strings.ToLower(r.Pattern), strings.ToLower(name)); err == nil && ok {
			return r, true
		}
	}
	return EntryRule{}, false
}

// entryState is what CheckEntries reads from an entry.
type entryState struct {
	ACL    aclState
	IsDir  bool
	IsLink bool
	Links  uint32
}

// Well-known SIDs that only matter in inheritable ACEs.
const (
	sidCreatorOwner = "S-1-3-0"
	sidCreatorGroup = "S-1-3-1"
)

// Deny ACE types that root.go doesn't need.
const (
	aceTypeAccessDeniedObject         = 0x6
	aceTypeAccessDeniedCallbackObject = 0xc
)

// Generic access rights that only allow reading.
const (
	accessGenericRead    uint32 = 0x80000000
	accessGenericExecute uint32 = 0x20000000
)

// entryReasons lists why an entry with state s isn't trusted, or nothing if
// it is: it is a reparse point, it has more than one hard link, its owner
// isn't trusted, or its DACL lets someone else change it.
func entryReasons(s entryState, trusted func(sid string) bool) []string {
	var reasons []string
	if s.IsLink {
		reasons = append(reasons, "is a junction, symbolic link or other reparse point")
	}
	if s.Links > 1 {
		reasons = append(reasons, fmt.Sprintf("has %d hard links", s.Links))
	}
	ownerTrusted := trusted(s.ACL.OwnerSID)
	if !ownerTrusted {
		reasons = append(reasons, fmt.Sprintf("isn't owned by Administrators, SYSTEM or a member of Administrators (its owner is %s)", sidName(s.ACL.OwnerSID)))
	}
	return append(reasons, aclReasons(s.ACL, s.IsDir, ownerTrusted, trusted)...)
}

// aclReasons lists who, other than trusted principals, the DACL of s lets
// change the entry: everyone for a NULL DACL, and each SID with an allow ACE,
// explicit or inherited, that grants more than readMask. ACEs that only apply
// to children are ignored on a file; on a directory, they count unless they
// are for CREATOR OWNER or CREATOR GROUP. OWNER RIGHTS counts only when the
// owner isn't trusted.
func aclReasons(s aclState, isDir, ownerTrusted bool, trusted func(sid string) bool) []string {
	if s.NullDACL {
		return []string{"has no access list, so everyone can change it"}
	}
	var sids []string
	for _, a := range s.ACEs {
		switch a.Type {
		case aceTypeAccessDenied, aceTypeAccessDeniedCallback, aceTypeAccessDeniedObject, aceTypeAccessDeniedCallbackObject:
			continue
		}
		if a.Flags&aceFlagInheritOnly != 0 && (!isDir || a.SID == sidCreatorOwner || a.SID == sidCreatorGroup) {
			continue
		}
		if a.Mask&^(readMask|accessGenericRead|accessGenericExecute) == 0 {
			continue
		}
		if a.SID != "" && (trusted(a.SID) || (a.SID == sidOwnerRights && ownerTrusted)) {
			continue
		}
		if r := sidName(a.SID); !slices.Contains(sids, r) {
			sids = append(sids, r)
		}
	}
	if len(sids) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("lets %s change it", strings.Join(sids, ", "))}
}

// entryOp is what CheckEntries does with an entry.
type entryOp int

const (
	opKeep entryOp = iota
	opQuarantine
	opRemove
)

// planEntry decides what to do with an entry that rule action applies to,
// and whether the run fails.
func planEntry(action EntryAction, isLink, untrusted bool) (op entryOp, fail bool) {
	if !untrusted {
		return opKeep, false
	}
	switch action {
	case EntryFail:
		return opKeep, true
	case EntryQuarantine:
		if isLink {
			return opRemove, true
		}
		return opQuarantine, true
	case EntryRemove:
		return opRemove, false
	case EntryRemoveLink:
		if isLink {
			return opRemove, false
		}
		return opKeep, false
	}
	return opKeep, true
}

// quarantinePath returns the path that an entry at path is renamed to. It
// doesn't end in the entry's extension, so googet ignores it.
func quarantinePath(path string, now time.Time) string {
	return fmt.Sprintf("%s.untrusted-%d", path, now.UnixNano())
}

// failureMessage describes an entry at path that isn't trusted for reasons,
// what was done with it and how to fix it.
func failureMessage(path string, reasons []string, op entryOp, newPath string, state entryState) string {
	what := fmt.Sprintf("%s %s", path, strings.Join(reasons, ", "))
	hardLinked := state.Links > 1
	switch op {
	case opQuarantine:
		if hardLinked {
			return fmt.Sprintf("%s, so googet renamed it to %s and didn't use it; check it and, if it is correct, copy its contents to a new file at %s (or remove the extra hard links first) with owner Administrators and only inherited permissions, then run googet again", what, newPath, path)
		}
		return fmt.Sprintf("%s, so googet renamed it to %s and didn't use it; check it and, if it is correct, rename it back and give it owner Administrators and only inherited permissions, then run googet again", what, newPath)
	case opRemove:
		return fmt.Sprintf("%s, so googet removed it and didn't use it; check the configuration, then run googet again", what)
	}
	if state.IsLink {
		return fmt.Sprintf("%s; check it, replace the link with a regular file with owner Administrators and only inherited permissions, or remove it, and run googet again", what)
	}
	if hardLinked {
		return fmt.Sprintf("%s; check it, remove the extra hard links or copy it to a new file with owner Administrators and only inherited permissions, or remove it, and run googet again", what)
	}
	return fmt.Sprintf("%s; check it, then give it owner Administrators and only inherited permissions, or remove it, and run googet again", what)
}

// entryKey returns a normalized case-insensitive key for path.
func entryKey(path string) string {
	return strings.ToLower(filepath.Clean(path))
}

// entryPathKey returns the path-based fallback key for path in
// RootReport.untrustedBefore.
func entryPathKey(path string) string {
	return "path:" + entryKey(path)
}

// combineReasons returns the reasons why an entry isn't trusted now, followed
// by those why it wasn't before PrepareRoot reset the permissions above it,
// unless they still apply.
func combineReasons(now, before []string) []string {
	reasons := slices.Clone(now)
	for _, reason := range before {
		if !slices.Contains(now, reason) {
			reasons = append(reasons, reason+" (before googet reset the permissions of the googet root)")
		}
	}
	return reasons
}

// recordBeforeKey records reasons for key in untrustedBefore.
func (r *RootReport) recordBeforeKey(key string, reasons []string) {
	if len(reasons) == 0 {
		return
	}
	if r.untrustedBefore == nil {
		r.untrustedBefore = make(map[string][]string)
	}
	r.untrustedBefore[key] = reasons
}

// beforeReasons returns any pre-repair untrusted reasons recorded for idKey
// or path.
func (r RootReport) beforeReasons(idKey, path string) []string {
	var reasons []string
	if idKey != "" {
		reasons = append(reasons, r.untrustedBefore[idKey]...)
	}
	for _, reason := range r.untrustedBefore[entryPathKey(path)] {
		if !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}
	return reasons
}
