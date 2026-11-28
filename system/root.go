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
	"slices"
	"strings"
)

const (
	// rootSDDL is the security descriptor of the googet root on Windows: owned
	// by Administrators, with a protected DACL that gives Administrators and
	// SYSTEM full control and Users read and execute access. canonicalACEs
	// describes the same DACL; a Windows test checks that they match.
	rootSDDL = "O:BAG:SYD:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)"

	// tempLockDACLSDDL is a non-inheritable protected DACL (no OICI, no AI)
	// applied via SetKernelObjectSecurity before recording children and
	// running SetSecurityInfo. It blocks untrusted writes on the directory
	// itself without propagating to children, and because it omits OICI, both
	// classifyRoot and subdirNeedsReset still trigger full repair and
	// propagation if the process exits before SetSecurityInfo completes.
	tempLockDACLSDDL = "D:P(A;;FA;;;BA)(A;;FA;;;SY)(A;;0x1200a9;;;BU)"
	// tempLockSDDL pairs the Administrators owner with tempLockDACLSDDL for
	// locking subdirectories without propagating inheritance.
	tempLockSDDL = "O:BA" + tempLockDACLSDDL
)

// Well-known SIDs.
const (
	sidAdministrators   = "S-1-5-32-544"
	sidSystem           = "S-1-5-18"
	sidTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	sidUsers            = "S-1-5-32-545"
	sidOwnerRights      = "S-1-3-4"
)

// readMask is the most access that a SID outside trustedSIDs may hold on the
// root: read data, read attributes and extended attributes, list, traverse,
// read permissions and synchronize.
const readMask uint32 = 0x1200a9

// ACE types, ACE flags and access rights from the Windows SDK.
const (
	aceTypeAccessAllowed               = 0x0
	aceTypeAccessDenied                = 0x1
	aceTypeAccessAllowedObject         = 0x5
	aceTypeAccessAllowedCallback       = 0x9
	aceTypeAccessDeniedCallback        = 0xa
	aceTypeAccessAllowedCallbackObject = 0xb

	aceFlagObjectInherit      = 0x1
	aceFlagContainerInherit   = 0x2
	aceFlagNoPropagateInherit = 0x4
	aceFlagInheritOnly        = 0x8
	aceFlagInherited          = 0x10
	aceFlagsInheritance       = aceFlagObjectInherit | aceFlagContainerInherit | aceFlagNoPropagateInherit | aceFlagInheritOnly

	accessFileAllAccess   uint32 = 0x1f01ff
	accessFileDeleteChild uint32 = 0x40
	accessDelete          uint32 = 0x10000
	accessWriteDAC        uint32 = 0x40000
	accessWriteOwner      uint32 = 0x80000
	accessGenericAll      uint32 = 0x10000000

	// accessAncestorRisk is the access on a parent of the root that lets its
	// holder replace the parent, and so the path to the root.
	accessAncestorRisk = accessDelete | accessFileDeleteChild | accessWriteDAC | accessWriteOwner | accessGenericAll
)

// trustedSIDs may hold any access on the root.
var trustedSIDs = map[string]bool{
	sidAdministrators:   true,
	sidSystem:           true,
	sidTrustedInstaller: true,
}

// canonicalACEs is the DACL of rootSDDL.
var canonicalACEs = []aceInfo{
	{Type: aceTypeAccessAllowed, Flags: aceFlagObjectInherit | aceFlagContainerInherit, Mask: accessFileAllAccess, SID: sidAdministrators},
	{Type: aceTypeAccessAllowed, Flags: aceFlagObjectInherit | aceFlagContainerInherit, Mask: accessFileAllAccess, SID: sidSystem},
	{Type: aceTypeAccessAllowed, Flags: aceFlagObjectInherit | aceFlagContainerInherit, Mask: readMask, SID: sidUsers},
}

// aceInfo is an access control entry. SID is empty for ACE types whose SID
// isn't read.
type aceInfo struct {
	Type  uint8
	Flags uint8
	Mask  uint32
	SID   string
}

// aclState is the owner and DACL of a security descriptor. NullDACL is set
// when the DACL is NULL or absent, both of which grant everyone full access.
type aclState struct {
	OwnerSID  string
	Protected bool
	NullDACL  bool
	ACEs      []aceInfo
}

// rootPlan says how to repair the root's security descriptor.
type rootPlan struct {
	// ResetOwner is set when the owner must be set to Administrators.
	ResetOwner bool
	// ReplaceDACL is set when the DACL must be replaced by DACL, protected.
	ReplaceDACL bool
	DACL        []aceInfo
}

func (p rootPlan) needed() bool {
	return p.ResetOwner || p.ReplaceDACL
}

// classifyRoot decides whether the root's descriptor s is trusted and, if
// not, how to repair it. It is trusted when its owner is trusted, its DACL is
// present, not NULL and protected, it contains canonicalACEs, and all of its
// ACEs are plain allow ACEs where only trustedSIDs hold more than readMask.
// The repaired DACL is canonicalACEs followed by the read-only allow ACEs of
// s, made explicit.
func classifyRoot(s aclState, trustedOwner func(sid string) bool) rootPlan {
	var p rootPlan
	p.ResetOwner = !trustedOwner(s.OwnerSID)
	if !rootDACLTrusted(s) {
		p.ReplaceDACL = true
		p.DACL = repairedDACL(s)
	}
	return p
}

func rootDACLTrusted(s aclState) bool {
	if s.NullDACL || !s.Protected {
		return false
	}
	for _, a := range s.ACEs {
		if a.Type != aceTypeAccessAllowed || a.Flags&^aceFlagsInheritance != 0 || isCreatorAuthoritySID(a.SID) {
			return false
		}
		if !trustedSIDs[a.SID] && a.Mask&^readMask != 0 {
			return false
		}
	}
	return hasCanonicalACEs(s.ACEs)
}

// hasCanonicalACEs reports whether aces contains every entry of canonicalACEs.
func hasCanonicalACEs(aces []aceInfo) bool {
	for _, c := range canonicalACEs {
		if !slices.Contains(aces, c) {
			return false
		}
	}
	return true
}

// isCreatorAuthoritySID reports whether sid is a SECURITY_CREATOR_SID_AUTHORITY
// pseudo-SID (such as CREATOR OWNER S-1-3-0 or OWNER RIGHTS S-1-3-4), which
// alters ownership or inheritance semantics rather than naming a principal.
func isCreatorAuthoritySID(sid string) bool {
	return strings.HasPrefix(sid, "S-1-3-")
}

func repairedDACL(s aclState) []aceInfo {
	dacl := slices.Clone(canonicalACEs)
	if s.NullDACL {
		return dacl
	}
	for _, a := range s.ACEs {
		// Administrators, SYSTEM and Users are already granted OICI coverage
		// by canonicalACEs at equal or higher access, so skip their read-only
		// ACEs (including non-inheritable ones from tempLockDACLSDDL).
		if a.Type != aceTypeAccessAllowed || a.SID == "" || isCreatorAuthoritySID(a.SID) ||
			a.SID == sidAdministrators || a.SID == sidSystem || a.SID == sidUsers ||
			a.Mask == 0 || a.Mask&^readMask != 0 {
			continue
		}
		a.Flags &= aceFlagsInheritance
		if a.Flags&(aceFlagObjectInherit|aceFlagContainerInherit) == 0 {
			if a.Flags&aceFlagInheritOnly != 0 {
				// An inherit-only ACE that is never inherited has no effect.
				continue
			}
			a.Flags &^= aceFlagNoPropagateInherit
		}
		if !slices.Contains(dacl, a) {
			dacl = append(dacl, a)
		}
	}
	return dacl
}

// subdirNeedsReset reports whether a directory under the root must be reset
// to inherit only from the root: its owner isn't trusted, its DACL is NULL,
// it is protected without the canonical inheritable ACEs (for example from a
// temporary non-propagating lock), or any ACE, explicit or inherited, isn't a
// plain allow ACE where only trustedSIDs hold more than readMask.
func subdirNeedsReset(s aclState, trustedOwner func(sid string) bool) bool {
	if !trustedOwner(s.OwnerSID) || s.NullDACL {
		return true
	}
	if s.Protected && !hasCanonicalACEs(s.ACEs) {
		return true
	}
	for _, a := range s.ACEs {
		if a.Type != aceTypeAccessAllowed || isCreatorAuthoritySID(a.SID) {
			return true
		}
		if !trustedSIDs[a.SID] && a.Mask&^readMask != 0 {
			return true
		}
	}
	return false
}

// inheritsCanonical reports whether s holds canonicalACEs as inherited ACEs,
// as a directory under the root does after it is reset.
func inheritsCanonical(s aclState) bool {
	if s.NullDACL || s.Protected {
		return false
	}
	for _, c := range canonicalACEs {
		c.Flags |= aceFlagInherited
		if !slices.Contains(s.ACEs, c) {
			return false
		}
	}
	return true
}

// ancestorRisks lists who, other than trusted principals, can replace a
// parent of the root with descriptor s: an untrusted owner, everyone for a
// NULL DACL, and SIDs with an allow ACE that applies to the parent itself
// and grants delete, delete child, write DAC or write owner.
func ancestorRisks(s aclState, trusted func(sid string) bool) []string {
	var risks []string
	ownerTrusted := trusted(s.OwnerSID)
	if !ownerTrusted {
		risks = append(risks, "owner "+sidName(s.OwnerSID))
	}
	if s.NullDACL {
		return append(risks, "everyone (no access list)")
	}
	for _, a := range s.ACEs {
		switch a.Type {
		case aceTypeAccessAllowed, aceTypeAccessAllowedObject, aceTypeAccessAllowedCallback, aceTypeAccessAllowedCallbackObject:
		default:
			continue
		}
		if a.Flags&aceFlagInheritOnly != 0 || a.Mask&accessAncestorRisk == 0 {
			continue
		}
		if trusted(a.SID) || (a.SID == sidOwnerRights && ownerTrusted) {
			continue
		}
		r := sidName(a.SID)
		if !slices.Contains(risks, r) {
			risks = append(risks, r)
		}
	}
	return risks
}

func sidName(sid string) string {
	if sid == "" {
		return "an unknown SID"
	}
	return sid
}

// daclSDDL returns the SDDL of a protected DACL with the allow ACEs aces.
func daclSDDL(aces []aceInfo) (string, error) {
	return formatDACLSDDL("D:PAI", aces)
}

// tempLockDACL returns the SDDL of tempLockDACLSDDL followed by any extra
// non-canonical allow ACEs from planDACL. Applied with SetKernelObjectSecurity,
// it blocks untrusted writes on the directory itself without propagating
// inheritance, preserves custom read-only ACEs across an interrupted repair,
// and keeps the canonical principals non-inheritable so classifyRoot still
// triggers full repair and propagation on retry.
func tempLockDACL(planDACL []aceInfo) (string, error) {
	var extra []aceInfo
	for _, a := range planDACL {
		if a.SID == sidAdministrators || a.SID == sidSystem || a.SID == sidUsers {
			continue
		}
		extra = append(extra, a)
	}
	return formatDACLSDDL(tempLockDACLSDDL, extra)
}

func formatDACLSDDL(prefix string, aces []aceInfo) (string, error) {
	var b strings.Builder
	b.WriteString(prefix)
	for _, a := range aces {
		if a.Type != aceTypeAccessAllowed {
			return "", fmt.Errorf("ACE type %#x isn't an allow ACE", a.Type)
		}
		if !validSID(a.SID) {
			return "", fmt.Errorf("invalid SID %q", a.SID)
		}
		var flags string
		for _, f := range []struct {
			bit  uint8
			sddl string
		}{
			{aceFlagObjectInherit, "OI"},
			{aceFlagContainerInherit, "CI"},
			{aceFlagNoPropagateInherit, "NP"},
			{aceFlagInheritOnly, "IO"},
		} {
			if a.Flags&f.bit != 0 {
				flags += f.sddl
			}
		}
		if a.Flags&^aceFlagsInheritance != 0 {
			return "", fmt.Errorf("unsupported ACE flags %#x", a.Flags)
		}
		fmt.Fprintf(&b, "(A;%s;%#x;;;%s)", flags, a.Mask, a.SID)
	}
	return b.String(), nil
}

// validSID reports whether sid is a SID in string form, such as S-1-5-18.
func validSID(sid string) bool {
	parts := strings.Split(sid, "-")
	if len(parts) < 3 || parts[0] != "S" {
		return false
	}
	for _, p := range parts[1:] {
		if p == "" {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// RootReport describes what PrepareRoot did. Its fields are populated on
// Windows; on other systems PrepareRoot returns an empty RootReport.
type RootReport struct {
	// Created is set when PrepareRoot created the root.
	Created bool
	// OwnerReset is set when the root's owner was set to Administrators.
	OwnerReset bool
	// DACLReplaced is set when the root's DACL was replaced.
	DACLReplaced bool
	// RemovedLinks lists the junctions, symbolic links and other reparse
	// points that were removed from the root's subdirectory paths.
	RemovedLinks []string
	// RepairedSubdirs lists the subdirectories whose owner and DACL were
	// reset to inherit from the root.
	RepairedSubdirs []string
	// Warnings lists problems that PrepareRoot found but didn't change.
	Warnings []string

	// untrustedBefore maps the file-ID key (or path key when an entry couldn't
	// be inspected) of each entry under the root that wasn't trusted before
	// PrepareRoot changed the permissions above it to the reasons, so that
	// CheckEntries still sees access that the change replaced.
	untrustedBefore map[string][]string
}

// Changed reports whether PrepareRoot changed an existing root. Creating a
// new root isn't a change.
func (r RootReport) Changed() bool {
	return r.OwnerReset || r.DACLReplaced || len(r.RemovedLinks) > 0 || len(r.RepairedSubdirs) > 0
}

// Summary describes the changes that Changed reports.
func (r RootReport) Summary() string {
	var parts []string
	if r.OwnerReset {
		parts = append(parts, "set the owner to Administrators")
	}
	if r.DACLReplaced {
		parts = append(parts, "replaced the access list")
	}
	if len(r.RemovedLinks) > 0 {
		parts = append(parts, fmt.Sprintf("removed links %q", r.RemovedLinks))
	}
	if len(r.RepairedSubdirs) > 0 {
		parts = append(parts, fmt.Sprintf("reset the permissions of %q", r.RepairedSubdirs))
	}
	return strings.Join(parts, "; ")
}
