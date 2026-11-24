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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/windows"
)

// usersOwner makes Users the owner, which CheckEntries doesn't trust.
const usersOwner = "O:BU"

// newEntriesRoot returns a prepared googet root with repos and cache.
func newEntriesRoot(t *testing.T) string {
	t.Helper()
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	prepareRoot(t, root)
	for _, name := range testSubdirs {
		mkdir(t, filepath.Join(root, name))
	}
	return root
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0666); err != nil {
		t.Fatal(err)
	}
}

func checkEntriesOK(t *testing.T, root string, prep RootReport) EntryReport {
	t.Helper()
	r, err := CheckEntries(root, testEntryRules, prep)
	if err != nil {
		t.Fatalf("CheckEntries(%q) failed: %v", root, err)
	}
	return r
}

func checkMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%q still exists (Lstat error: %v)", path, err)
	}
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func checkContent(t *testing.T, path, want string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Errorf("ReadFile(%q) = %q, %v; want %q", path, got, err, want)
	}
}

// fileSnapshot is the descriptor and modification time of a file.
type fileSnapshot struct {
	sd    []byte
	mtime time.Time
}

func snapshot(t *testing.T, path string) fileSnapshot {
	t.Helper()
	_, sd := descriptor(t, path)
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileSnapshot{sd: sd, mtime: fi.ModTime()}
}

func checkSnapshot(t *testing.T, path string, before fileSnapshot) {
	t.Helper()
	checkUnchanged(t, path, before.sd)
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(before.mtime) {
		t.Errorf("modification time of %q changed from %v to %v", path, before.mtime, fi.ModTime())
	}
}

// checkQuarantined checks that r reports path as renamed and failing, and
// that the renamed file has data.
func checkQuarantined(t *testing.T, r EntryReport, path, data string) {
	t.Helper()
	newPath, ok := r.Quarantined[path]
	if !ok {
		t.Fatalf("CheckEntries = %+v, want %q renamed", r, path)
	}
	if filepath.Dir(newPath) != filepath.Dir(path) || !strings.HasPrefix(filepath.Base(newPath), filepath.Base(path)+".untrusted-") {
		t.Errorf("%q renamed to %q, want %s.untrusted-<n> in the same directory", path, newPath, filepath.Base(path))
	}
	checkMissing(t, path)
	checkContent(t, newPath, data)
	if len(r.Failures) != 1 || !strings.Contains(r.Failures[0], path) || !strings.Contains(r.Failures[0], newPath) {
		t.Errorf("CheckEntries failures = %q, want one about %q and %q", r.Failures, path, newPath)
	}
}

func currentUserSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

func TestCheckEntriesQuarantinesUntrustedConf(t *testing.T) {
	root := newEntriesRoot(t)
	conf := filepath.Join(root, "googet.conf")
	writeFile(t, conf, "proxyserver: x")
	setDescriptor(t, conf, usersOwner)
	r := checkEntriesOK(t, root, RootReport{})
	checkQuarantined(t, r, conf, "proxyserver: x")
	if len(r.Removed) > 0 {
		t.Errorf("CheckEntries removed %q, want nothing removed", r.Removed)
	}
	// The renamed file matches no rule, so the next check passes.
	if r := checkEntriesOK(t, root, RootReport{}); r.Changed() || r.Failed() {
		t.Errorf("second CheckEntries = %+v, want no change", r)
	}
}

func TestCheckEntriesQuarantinesUntrustedRepo(t *testing.T) {
	root := newEntriesRoot(t)
	repo := filepath.Join(root, "repos", "x.repo")
	writeFile(t, repo, "- name: x")
	setDescriptor(t, repo, usersOwner)
	r := checkEntriesOK(t, root, RootReport{})
	checkQuarantined(t, r, repo, "- name: x")
	// This is the pattern that repo.ConfigFiles reads.
	if got, err := filepath.Glob(filepath.Join(root, "repos", "*.repo")); err != nil || len(got) > 0 {
		t.Errorf("Glob(*.repo) = %q, %v; want no repo files", got, err)
	}
}

func TestCheckEntriesFailsOnUntrustedDatabase(t *testing.T) {
	for _, name := range []string{"googet.db", "googet.db-journal", "googet.db-wal", "googet.db-shm", "googet.state", "googet.state.bak"} {
		t.Run(name, func(t *testing.T) {
			root := newEntriesRoot(t)
			path := filepath.Join(root, name)
			writeFile(t, path, "data")
			setDescriptor(t, path, usersOwner)
			before := snapshot(t, path)
			r := checkEntriesOK(t, root, RootReport{})
			if r.Changed() || len(r.Failures) != 1 || !strings.Contains(r.Failures[0], path) || !strings.Contains(r.Failures[0], sidUsers) {
				t.Errorf("CheckEntries = %+v, want one failure about %q and %s and no change", r, path, sidUsers)
			}
			checkContent(t, path, "data")
			checkSnapshot(t, path, before)
		})
	}
}

func TestCheckEntriesRemovesUntrustedLockAndLogs(t *testing.T) {
	root := newEntriesRoot(t)
	var paths []string
	for _, name := range []string{"googet.lock", "googet.log", "googet.log.old", "googet.log.old.2"} {
		p := filepath.Join(root, name)
		writeFile(t, p, "1234")
		setDescriptor(t, p, usersOwner)
		paths = append(paths, p)
	}
	r := checkEntriesOK(t, root, RootReport{})
	if diff := cmp.Diff(sortedCopy(paths), sortedCopy(r.Removed)); diff != "" || r.Failed() {
		t.Errorf("CheckEntries = %+v, want the lock and logs removed and no failure; Removed mismatch (-want +got):\n%s", r, diff)
	}
	for _, p := range paths {
		checkMissing(t, p)
	}
}

func TestCheckEntriesRemovesUntrustedCacheEntries(t *testing.T) {
	root := newEntriesRoot(t)
	cache := filepath.Join(root, "cache")
	pkg := filepath.Join(cache, "pkg.x86_64.1.0.goo")
	writeFile(t, pkg, "goo")
	setDescriptor(t, pkg, usersOwner)
	tree := filepath.Join(cache, "tree")
	mkdir(t, tree)
	mkdir(t, filepath.Join(tree, "sub"))
	readOnly := filepath.Join(tree, "sub", "file")
	writeFile(t, readOnly, "x")
	if err := os.Chmod(readOnly, 0444); err != nil {
		t.Fatal(err)
	}
	setDescriptor(t, tree, usersOwner)
	kept := filepath.Join(cache, "kept.goo")
	writeFile(t, kept, "kept")
	r := checkEntriesOK(t, root, RootReport{})
	if diff := cmp.Diff(sortedCopy([]string{pkg, tree}), sortedCopy(r.Removed)); diff != "" || r.Failed() {
		t.Errorf("CheckEntries = %+v, want the untrusted cache entries removed and no failure; Removed mismatch (-want +got):\n%s", r, diff)
	}
	checkMissing(t, pkg)
	checkMissing(t, tree)
	checkContent(t, kept, "kept")
}

func TestCheckEntriesRemovesLinks(t *testing.T) {
	root := newEntriesRoot(t)
	target := t.TempDir()
	sentinel := filepath.Join(target, "sentinel.repo")
	writeFile(t, sentinel, "- name: x")
	targetBefore := snapshot(t, target)
	sentinelBefore := snapshot(t, sentinel)
	repoLink := filepath.Join(root, "repos", "linked.repo")
	otherLink := filepath.Join(root, "repos", "other")
	cacheLink := filepath.Join(root, "cache", "linked")
	for _, l := range []string{repoLink, otherLink, cacheLink} {
		mklink(t, l, target)
	}
	fileLink := filepath.Join(root, "cache", "file.goo")
	if out, err := exec.Command("cmd", "/c", "mklink", fileLink, sentinel).CombinedOutput(); err != nil {
		t.Fatalf("mklink %q %q: %v\n%s", fileLink, sentinel, err, out)
	}
	r := checkEntriesOK(t, root, RootReport{})
	want := []string{repoLink, otherLink, fileLink, cacheLink}
	if diff := cmp.Diff(sortedCopy(want), sortedCopy(r.Removed)); diff != "" {
		t.Errorf("CheckEntries Removed mismatch (-want +got):\n%s", diff)
	}
	if len(r.Failures) != 1 || !strings.Contains(r.Failures[0], repoLink) {
		t.Errorf("CheckEntries failures = %q, want one about %q", r.Failures, repoLink)
	}
	for _, l := range want {
		checkMissing(t, l)
	}
	checkContent(t, sentinel, "- name: x")
	checkSnapshot(t, target, targetBefore)
	checkSnapshot(t, sentinel, sentinelBefore)
}

func TestCheckEntriesTreatsHardLinkAsUntrusted(t *testing.T) {
	root := newEntriesRoot(t)
	other := filepath.Join(t.TempDir(), "other")
	writeFile(t, other, "proxyserver: x")
	conf := filepath.Join(root, "googet.conf")
	if err := os.Link(other, conf); err != nil {
		t.Fatal(err)
	}
	r := checkEntriesOK(t, root, RootReport{})
	checkQuarantined(t, r, conf, "proxyserver: x")
	if !strings.Contains(r.Failures[0], "2 hard links") {
		t.Errorf("CheckEntries failure = %q, want it to mention the hard links", r.Failures[0])
	}
	checkContent(t, other, "proxyserver: x")
}

func TestCheckEntriesTreatsUsersWriteACEAsUntrusted(t *testing.T) {
	root := newEntriesRoot(t)
	conf := filepath.Join(root, "googet.conf")
	writeFile(t, conf, "x")
	setDescriptor(t, conf, "O:BAD:AI(A;;0x1301bf;;;BU)")
	if s, _ := descriptor(t, conf); s.OwnerSID != sidAdministrators {
		t.Fatalf("precondition: owner of %q = %s, want %s", conf, s.OwnerSID, sidAdministrators)
	}
	r := checkEntriesOK(t, root, RootReport{})
	checkQuarantined(t, r, conf, "x")
	if !strings.Contains(r.Failures[0], sidUsers) {
		t.Errorf("CheckEntries failure = %q, want it to mention %s", r.Failures[0], sidUsers)
	}
}

func TestCheckEntriesKeepsAdministratorOwnedFiles(t *testing.T) {
	root := newEntriesRoot(t)
	// The owner is the elevated user, as under the "Object creator" default
	// owner policy.
	owner := "O:" + currentUserSID(t)
	for _, p := range []string{
		filepath.Join(root, "googet.conf"),
		filepath.Join(root, "googet.db"),
		filepath.Join(root, "googet.lock"),
		filepath.Join(root, "repos", "x.repo"),
		filepath.Join(root, "cache", "x.goo"),
	} {
		writeFile(t, p, "x")
		setDescriptor(t, p, owner)
		defer checkContent(t, p, "x")
	}
	if r := checkEntriesOK(t, root, RootReport{}); r.Changed() || r.Failed() {
		t.Errorf("CheckEntries = %+v, want no change", r)
	}
}

func TestCheckEntriesLeavesHealthyRootUnchanged(t *testing.T) {
	root := newEntriesRoot(t)
	var paths []string
	for _, name := range []string{"googet.conf", "googet.db", "googet.db-journal", "googet.lock", "googet.log", "googet.log.old", "googet.state", `repos\x.repo`, `repos\notes.txt`, `cache\x.goo`} {
		p := filepath.Join(root, name)
		writeFile(t, p, "x")
		paths = append(paths, p)
	}
	mkdir(t, filepath.Join(root, "cache", "dir"))
	paths = append(paths, root, filepath.Join(root, "repos"), filepath.Join(root, "cache"), filepath.Join(root, "cache", "dir"))
	var before []fileSnapshot
	for _, p := range paths {
		before = append(before, snapshot(t, p))
	}
	if r := checkEntriesOK(t, root, RootReport{}); r.Changed() || r.Failed() {
		t.Errorf("CheckEntries = %+v, want no change", r)
	}
	for i, p := range paths {
		checkSnapshot(t, p, before[i])
	}
}

func TestCheckEntriesUsesPermissionsBeforeRootRepair(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	mkdir(t, root)
	// Users can change everything in the root; the entries below inherit
	// that and are owned by Administrators.
	setDescriptor(t, root, "D:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;BU)")
	mkdir(t, filepath.Join(root, "repos"))
	conf := filepath.Join(root, "googet.conf")
	repo := filepath.Join(root, "repos", "x.repo")
	db := filepath.Join(root, "googet.db")
	for _, p := range []string{conf, repo, db} {
		writeFile(t, p, "x")
		if s, _ := descriptor(t, p); !trustedOwner(s.OwnerSID) {
			t.Fatalf("precondition: owner of %q = %s, want a trusted owner", p, s.OwnerSID)
		}
	}
	prep := prepareRoot(t, root)
	if !prep.DACLReplaced {
		t.Fatalf("PrepareRoot(%q) = %+v, want the DACL replaced", root, prep)
	}
	for _, p := range []string{conf, repo, db} {
		info, err := inspectEntry(p)
		if err != nil || info == nil {
			t.Fatalf("inspectEntry(%q) = %v, %v", p, info, err)
		}
		if !info.id.is128 {
			t.Errorf("fileIdentity(%q) did not use 128-bit FileIdInfo", p)
		}
		if got := prep.beforeReasons(info.id.key(), p); len(got) == 0 {
			t.Errorf("PrepareRoot didn't record that %q wasn't trusted before the repair (key %q)", p, info.id.key())
		}
	}
	// Verify file-ID tracking survives a rename: move repo to a new .repo name
	// before CheckEntries and confirm it is still recognized as untrusted.
	renamedRepo := filepath.Join(root, "repos", "moved.repo")
	if err := os.Rename(repo, renamedRepo); err != nil {
		t.Fatalf("Rename(%q, %q): %v", repo, renamedRepo, err)
	}
	r := checkEntriesOK(t, root, prep)
	if _, ok := r.Quarantined[conf]; !ok {
		t.Errorf("CheckEntries = %+v, want %q renamed", r, conf)
	}
	if _, ok := r.Quarantined[renamedRepo]; !ok {
		t.Errorf("CheckEntries = %+v, want renamed %q quarantined via file ID", r, renamedRepo)
	}
	if len(r.Failures) != 3 {
		t.Errorf("CheckEntries failures = %q, want three", r.Failures)
	}
	for _, f := range r.Failures {
		if !strings.Contains(f, "before googet reset the permissions") {
			t.Errorf("failure = %q, want it to mention the pre-repair permissions", f)
		}
	}
	checkContent(t, db, "x")
}

func TestCheckEntriesWithoutRepairRecordsNothing(t *testing.T) {
	root := newEntriesRoot(t)
	writeFile(t, filepath.Join(root, "googet.conf"), "x")
	if prep := prepareRoot(t, root); prep.untrustedBefore != nil {
		t.Errorf("PrepareRoot on a prepared root recorded %v, want nothing", prep.untrustedBefore)
	}
}

func TestCheckEntriesWithoutRoot(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "googet")
	if r, err := CheckEntries(root, testEntryRules, RootReport{}); err == nil {
		t.Errorf("CheckEntries(%q) = %+v, want an error for a missing root", root, r)
	}
}

func TestCheckEntriesQuarantineNameCollisionDoesNotOverwrite(t *testing.T) {
	root := newEntriesRoot(t)
	origNow := now
	fixed := time.Unix(0, 9999)
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = origNow })

	conf := filepath.Join(root, "googet.conf")
	writeFile(t, conf, "incoming")
	setDescriptor(t, conf, usersOwner)

	existing := quarantinePath(conf, fixed)
	writeFile(t, existing, "existing")
	before := snapshot(t, existing)

	if r, err := CheckEntries(root, testEntryRules, RootReport{}); err == nil {
		t.Fatalf("CheckEntries with colliding target %q = %+v, want an error", existing, r)
	}
	checkContent(t, existing, "existing")
	checkSnapshot(t, existing, before)
	checkContent(t, conf, "incoming")
}

func TestCheckEntriesFileIDMismatchStopsRenameAndRemove(t *testing.T) {
	root := newEntriesRoot(t)
	conf := filepath.Join(root, "googet.conf")
	writeFile(t, conf, "v1")
	setDescriptor(t, conf, usersOwner)

	var info *entryInfo
	if err := withPrivileges(func() error {
		var err error
		info, err = inspectEntry(conf)
		return err
	}); err != nil || info == nil {
		t.Fatalf("inspectEntry(%q) = %v, %v; want entryInfo", conf, info, err)
	}

	// Replace the file so its volume file ID no longer matches info.
	if err := os.Remove(conf); err != nil {
		t.Fatal(err)
	}
	writeFile(t, conf, "v2")

	if err := withPrivileges(func() error {
		return quarantine(conf, info, "googet.conf.untrusted-1")
	}); err == nil || !strings.Contains(err.Error(), "changed while googet checked it") {
		t.Errorf("quarantine after file replacement = %v, want file-ID mismatch error", err)
	}
	checkContent(t, conf, "v2")

	if err := withPrivileges(func() error {
		return remove(conf, info)
	}); err == nil || !strings.Contains(err.Error(), "changed while googet checked it") {
		t.Errorf("remove after file replacement = %v, want file-ID mismatch error", err)
	}
	checkContent(t, conf, "v2")

	// Also verify openSame rejects a matching file ID when IsDir or IsLink
	// differs from what inspectEntry recorded.
	var current *entryInfo
	if err := withPrivileges(func() error {
		var err error
		current, err = inspectEntry(conf)
		return err
	}); err != nil || current == nil {
		t.Fatalf("inspectEntry(%q) = %v, %v; want entryInfo", conf, current, err)
	}
	for _, tc := range []struct {
		desc   string
		mutate func(*entryInfo)
	}{
		{"IsDir mismatch", func(e *entryInfo) { e.state.IsDir = true }},
		{"IsLink mismatch", func(e *entryInfo) { e.state.IsLink = true }},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			drifted := *current
			tc.mutate(&drifted)
			if err := withPrivileges(func() error {
				return quarantine(conf, &drifted, "googet.conf.untrusted-2")
			}); err == nil || !strings.Contains(err.Error(), "changed while googet checked it") {
				t.Errorf("quarantine with %s = %v, want type mismatch error", tc.desc, err)
			}
			checkContent(t, conf, "v2")
			if err := withPrivileges(func() error {
				return remove(conf, &drifted)
			}); err == nil || !strings.Contains(err.Error(), "changed while googet checked it") {
				t.Errorf("remove with %s = %v, want type mismatch error", tc.desc, err)
			}
			checkContent(t, conf, "v2")
		})
	}
}

func TestCheckEntriesRemovesUntrustedCacheTreeWithNestedLinks(t *testing.T) {
	root := newEntriesRoot(t)
	targetDir := t.TempDir()
	sentinel := filepath.Join(targetDir, "sentinel.txt")
	writeFile(t, sentinel, "keep")
	targetBefore := snapshot(t, targetDir)
	sentinelBefore := snapshot(t, sentinel)

	cache := filepath.Join(root, "cache")
	tree := filepath.Join(cache, "tree")
	sub := filepath.Join(tree, "sub")
	mkdir(t, tree)
	mkdir(t, sub)
	writeFile(t, filepath.Join(sub, "plain.txt"), "x")
	mklink(t, filepath.Join(sub, "nested_junction"), targetDir)
	if out, err := exec.Command("cmd", "/c", "mklink", "/D", filepath.Join(sub, "nested_dirlink"), targetDir).CombinedOutput(); err != nil {
		t.Fatalf("mklink /D: %v\n%s", err, out)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", filepath.Join(sub, "nested_filelink"), sentinel).CombinedOutput(); err != nil {
		t.Fatalf("mklink file: %v\n%s", err, out)
	}
	setDescriptor(t, tree, usersOwner)

	r := checkEntriesOK(t, root, RootReport{})
	if diff := cmp.Diff([]string{tree}, r.Removed); diff != "" || r.Failed() {
		t.Errorf("CheckEntries = %+v, want %q removed with no failure; Removed mismatch (-want +got):\n%s", r, tree, diff)
	}
	checkMissing(t, tree)
	checkContent(t, sentinel, "keep")
	checkSnapshot(t, targetDir, targetBefore)
	checkSnapshot(t, sentinel, sentinelBefore)
}

func TestRemoveDirectoryBlocksConcurrentRename(t *testing.T) {
	root := newEntriesRoot(t)
	cache := filepath.Join(root, "cache")
	tree := filepath.Join(cache, "tree")
	mkdir(t, tree)
	writeFile(t, filepath.Join(tree, "child.txt"), "x")
	setDescriptor(t, tree, usersOwner)

	access, share := removeAccess(true)
	if share&windows.FILE_SHARE_DELETE != 0 {
		t.Fatalf("removeAccess(true) share = %#x, want FILE_SHARE_DELETE omitted", share)
	}

	if err := withPrivileges(func() error {
		info, err := inspectEntry(tree)
		if err != nil {
			return err
		}
		h, err := openSame(tree, info, access, share)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		if err := os.Rename(tree, filepath.Join(cache, "renamed")); err == nil {
			t.Errorf("os.Rename(%q) succeeded while tree handle was open without FILE_SHARE_DELETE", tree)
		}
		return removeOpen(tree, h)
	}); err != nil {
		t.Fatalf("removing tree: %v", err)
	}
	checkMissing(t, tree)
}

func TestCheckEntriesHardLinksOnLogAndDB(t *testing.T) {
	root := newEntriesRoot(t)
	sentinelDir := t.TempDir()
	logSentinel := filepath.Join(sentinelDir, "log_sentinel")
	dbSentinel := filepath.Join(sentinelDir, "db_sentinel")
	writeFile(t, logSentinel, "log-data")
	writeFile(t, dbSentinel, "db-data")
	logBefore := snapshot(t, logSentinel)
	dbBefore := snapshot(t, dbSentinel)

	logPath := filepath.Join(root, "googet.log")
	dbPath := filepath.Join(root, "googet.db")
	if err := os.Link(logSentinel, logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(dbSentinel, dbPath); err != nil {
		t.Fatal(err)
	}

	r := checkEntriesOK(t, root, RootReport{})
	if diff := cmp.Diff([]string{logPath}, r.Removed); diff != "" {
		t.Errorf("CheckEntries Removed mismatch (-want +got):\n%s", diff)
	}
	checkMissing(t, logPath)
	if len(r.Failures) != 1 || !strings.Contains(r.Failures[0], dbPath) || !strings.Contains(r.Failures[0], "2 hard links") {
		t.Errorf("CheckEntries failures = %q, want one about %q and 2 hard links", r.Failures, dbPath)
	}
	checkContent(t, dbPath, "db-data")
	checkContent(t, logSentinel, "log-data")
	checkContent(t, dbSentinel, "db-data")
	checkSnapshot(t, logSentinel, logBefore)
	checkSnapshot(t, dbSentinel, dbBefore)
}

func TestCheckEntriesHeldLocks(t *testing.T) {
	t.Run("held trusted lock is kept", func(t *testing.T) {
		root := newEntriesRoot(t)
		lockPath := filepath.Join(root, "googet.lock")
		cleanup, err := ObtainLock(lockPath, time.Hour)
		if err != nil {
			t.Fatalf("ObtainLock(%q): %v", lockPath, err)
		}
		defer cleanup()

		r := checkEntriesOK(t, root, RootReport{})
		if r.Changed() || r.Failed() {
			t.Errorf("CheckEntries with held trusted lock = %+v, want no change and no failure", r)
		}
		if _, err := os.Lstat(lockPath); err != nil {
			t.Errorf("trusted lock file missing after CheckEntries: %v", err)
		}
	})

	t.Run("held untrusted lock fails closed", func(t *testing.T) {
		root := newEntriesRoot(t)
		lockPath := filepath.Join(root, "googet.lock")
		writeFile(t, lockPath, "99999")
		setDescriptor(t, lockPath, usersOwner)

		// os.OpenFile on Windows omits FILE_SHARE_DELETE, blocking removal.
		f, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
		if err != nil {
			t.Fatalf("opening untrusted lock: %v", err)
		}
		defer f.Close()

		if r, err := CheckEntries(root, testEntryRules, RootReport{}); err == nil {
			t.Errorf("CheckEntries with held untrusted lock = %+v, want an error", r)
		}
	})
}

func TestCheckEntriesPreRepairSubdirAndSkipsSubdirLinks(t *testing.T) {
	t.Run("canonical root with untrusted repos subdir records inherited ACE before reset", func(t *testing.T) {
		root := newEntriesRoot(t)
		repos := filepath.Join(root, "repos")
		// Give repos an explicit Users modify ACE while the root stays canonical.
		setDescriptor(t, repos, "D:AI(A;OICI;0x1301bf;;;BU)")
		repo := filepath.Join(repos, "x.repo")
		writeFile(t, repo, "- name: x")
		if s, _ := descriptor(t, repo); !trustedOwner(s.OwnerSID) {
			t.Fatalf("precondition: owner of %q = %s, want a trusted owner", repo, s.OwnerSID)
		}

		prep := prepareRoot(t, root)
		if prep.DACLReplaced {
			t.Fatalf("PrepareRoot(%q) replaced root DACL, want only subdir repair: %+v", root, prep)
		}
		if diff := cmp.Diff([]string{repos}, prep.RepairedSubdirs); diff != "" {
			t.Fatalf("PrepareRoot RepairedSubdirs mismatch (-want +got):\n%s", diff)
		}
		info, err := inspectEntry(repo)
		if err != nil || info == nil {
			t.Fatalf("inspectEntry(%q) = %v, %v", repo, info, err)
		}
		if got := prep.beforeReasons(info.id.key(), repo); len(got) == 0 {
			t.Fatalf("PrepareRoot didn't record pre-repair reasons for %q (key %q)", repo, info.id.key())
		}
		// Verify PrepareRoot propagated repos's repaired DACL to repo so only
		// prep's pre-repair record makes it untrusted.
		if s, _ := descriptor(t, repo); len(aclReasons(s, false, trustedOwner(s.OwnerSID), trustedOwner)) != 0 {
			t.Fatalf("after PrepareRoot, %q still has untrusted current ACL: %+v", repo, s)
		}

		r := checkEntriesOK(t, root, prep)
		checkQuarantined(t, r, repo, "- name: x")
		if !strings.Contains(r.Failures[0], "before googet reset the permissions") {
			t.Errorf("failure = %q, want it to mention the pre-repair permissions", r.Failures[0])
		}
	})

	t.Run("root repair skips subdir junction without reading target", func(t *testing.T) {
		requireElevated(t)
		root := filepath.Join(t.TempDir(), "googet")
		mkdir(t, root)
		setDescriptor(t, root, "D:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;BU)")
		target := t.TempDir()
		sentinel := filepath.Join(target, "x.repo")
		writeFile(t, sentinel, "- name: x")
		setDescriptor(t, sentinel, "D:PAI(A;;FA;;;BA)(A;;FA;;;SY)(A;;0x1301bf;;;BU)")
		targetBefore := snapshot(t, target)
		sentinelBefore := snapshot(t, sentinel)

		reposLink := filepath.Join(root, "repos")
		mklink(t, reposLink, target)

		prep := prepareRoot(t, root)
		if diff := cmp.Diff([]string{reposLink}, prep.RemovedLinks); diff != "" {
			t.Errorf("PrepareRoot RemovedLinks mismatch (-want +got):\n%s", diff)
		}
		sentinelInfo, err := inspectEntry(sentinel)
		if err != nil || sentinelInfo == nil {
			t.Fatalf("inspectEntry(%q) = %v, %v", sentinel, sentinelInfo, err)
		}
		if got := prep.beforeReasons(sentinelInfo.id.key(), filepath.Join(reposLink, "x.repo")); len(got) != 0 {
			t.Errorf("PrepareRoot recorded entry through junction: %q", got)
		}
		checkMissing(t, reposLink)
		checkSnapshot(t, target, targetBefore)
		checkSnapshot(t, sentinel, sentinelBefore)
	})
}

func TestCheckEntriesRefusesLinkOrFileAtRootAndSubdir(t *testing.T) {
	requireElevated(t)
	target := t.TempDir()
	sentinel := filepath.Join(target, "x.repo")
	writeFile(t, sentinel, "keep")
	targetBefore := snapshot(t, target)

	rootLink := filepath.Join(t.TempDir(), "root_link")
	mklink(t, rootLink, target)
	if r, err := CheckEntries(rootLink, testEntryRules, RootReport{}); err == nil {
		t.Errorf("CheckEntries on junction root = %+v, want an error", r)
	}
	checkSnapshot(t, target, targetBefore)

	root := newEntriesRoot(t)
	repos := filepath.Join(root, "repos")
	if err := os.Remove(repos); err != nil {
		t.Fatal(err)
	}
	mklink(t, repos, target)
	if r, err := CheckEntries(root, testEntryRules, RootReport{}); err == nil {
		t.Errorf("CheckEntries with junction repos = %+v, want an error", r)
	}
	checkSnapshot(t, target, targetBefore)
}

func TestClearReadOnly(t *testing.T) {
	requireElevated(t)
	p := filepath.Join(t.TempDir(), "ro.txt")
	writeFile(t, p, "x")
	if err := os.Chmod(p, 0444); err != nil {
		t.Fatal(err)
	}
	h, err := openNoFollow(p, changeAccess, shareAll)
	if err != nil {
		t.Fatalf("openNoFollow(%q): %v", p, err)
	}
	defer windows.CloseHandle(h)
	if err := clearReadOnly(h); err != nil {
		t.Fatalf("clearReadOnly(%q): %v", p, err)
	}
	var b fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&b)), uint32(unsafe.Sizeof(b))); err != nil {
		t.Fatalf("GetFileInformationByHandleEx: %v", err)
	}
	if b.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		t.Errorf("FILE_ATTRIBUTE_READONLY still set after clearReadOnly: %#x", b.FileAttributes)
	}
}

func TestPrepareRootRetryAfterInterruptedRepair(t *testing.T) {
	requireElevated(t)

	t.Run("root repair retry", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		mkdir(t, root)
		setDescriptor(t, root, "D:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;BU)")
		conf := filepath.Join(root, "googet.conf")
		writeFile(t, conf, "x")

		origWrite := writeSecurity
		writeSecurity = func(h windows.Handle, info windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
			return errors.New("injected repair failure")
		}
		t.Cleanup(func() { writeSecurity = origWrite })

		if _, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Fatalf("PrepareRoot(%q) succeeded with failing writeSecurity, want error", root)
		}
		// After the interrupted repair, the root has the non-inheritable temporary
		// lock DACL (so classifyRoot still requires ReplaceDACL) and conf still
		// retains its inherited Users-modify ACE (lockRootNoPropagate didn't
		// propagate).
		rs, _ := descriptor(t, root)
		if plan := classifyRoot(rs, trustedOwner); !plan.ReplaceDACL {
			t.Fatalf("after interrupted repair, classifyRoot = %+v, want ReplaceDACL=true", plan)
		}
		if cs, _ := descriptor(t, conf); len(aclReasons(cs, false, trustedOwner(cs.OwnerSID), trustedOwner)) == 0 {
			t.Fatalf("lockRootNoPropagate unexpectedly propagated to %q: %+v", conf, cs)
		}

		// Retry without the injected failure: PrepareRoot must restore the
		// exact canonical DACL (no duplicate BU ACE), propagate canonical ACEs
		// to conf, and record conf's pre-repair untrusted ACL.
		writeSecurity = origWrite
		prep := prepareRoot(t, root)
		if !prep.DACLReplaced {
			t.Errorf("retry PrepareRoot = %+v, want DACLReplaced=true", prep)
		}
		checkCanonicalRoot(t, root, "")
		if cs, _ := descriptor(t, conf); len(aclReasons(cs, false, trustedOwner(cs.OwnerSID), trustedOwner)) != 0 {
			t.Errorf("after retry PrepareRoot, %q still has untrusted ACL: %+v", conf, cs)
		}
		info, err := inspectEntry(conf)
		if err != nil || info == nil {
			t.Fatalf("inspectEntry(%q) = %v, %v", conf, info, err)
		}
		if got := prep.beforeReasons(info.id.key(), conf); len(got) == 0 {
			t.Errorf("retry PrepareRoot didn't record pre-repair reasons for %q", conf)
		}
	})

	t.Run("root repair retry preserves custom read-only ACE and reports partial owner reset", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		mkdir(t, root)
		setDescriptor(t, root, usersOwner+"D:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;BU)(A;OICI;0x1200a9;;;WD)")

		origWrite := writeSecurity
		writeSecurity = func(h windows.Handle, info windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
			return errors.New("injected repair failure")
		}
		t.Cleanup(func() { writeSecurity = origWrite })

		partial, err := PrepareRoot(root, testSubdirs)
		if err == nil {
			t.Fatalf("PrepareRoot(%q) succeeded with failing writeSecurity, want error", root)
		}
		if !partial.OwnerReset || partial.DACLReplaced {
			t.Errorf("partial PrepareRoot report = %+v, want OwnerReset=true, DACLReplaced=false", partial)
		}

		writeSecurity = origWrite
		prep := prepareRoot(t, root)
		if prep.OwnerReset || !prep.DACLReplaced {
			t.Errorf("retry PrepareRoot = %+v, want OwnerReset=false, DACLReplaced=true", prep)
		}
		checkCanonicalRoot(t, root, sidAdministrators, allow(oici, readMask, sidEveryone))
	})

	t.Run("subdir repair retry", func(t *testing.T) {
		root := newEntriesRoot(t)
		repos := filepath.Join(root, "repos")
		setDescriptor(t, repos, "D:AI(A;OICI;0x1301bf;;;BU)")
		repo := filepath.Join(repos, "x.repo")
		writeFile(t, repo, "- name: x")

		origWrite := writeSecurity
		writeSecurity = func(h windows.Handle, info windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
			return errors.New("injected subdir reset failure")
		}
		t.Cleanup(func() { writeSecurity = origWrite })

		if _, err := PrepareRoot(root, testSubdirs); err == nil {
			t.Fatalf("PrepareRoot(%q) succeeded with failing writeSecurity, want error", root)
		}
		if rs, _ := descriptor(t, repos); !subdirNeedsReset(rs, trustedOwner) {
			t.Fatalf("after interrupted subdir repair, subdirNeedsReset(%q) = false, want true: %+v", repos, rs)
		}
		if cs, _ := descriptor(t, repo); len(aclReasons(cs, false, trustedOwner(cs.OwnerSID), trustedOwner)) == 0 {
			t.Fatalf("lockDirNoPropagate unexpectedly propagated to %q: %+v", repo, cs)
		}

		writeSecurity = origWrite
		prep := prepareRoot(t, root)
		if diff := cmp.Diff([]string{repos}, prep.RepairedSubdirs); diff != "" {
			t.Errorf("retry PrepareRoot RepairedSubdirs mismatch (-want +got):\n%s", diff)
		}
		if rs, _ := descriptor(t, repos); subdirNeedsReset(rs, trustedOwner) || !inheritsCanonical(rs) {
			t.Errorf("after retry PrepareRoot, %q state = %+v, want canonical inheritance", repos, rs)
		}
		if cs, _ := descriptor(t, repo); len(aclReasons(cs, false, trustedOwner(cs.OwnerSID), trustedOwner)) != 0 {
			t.Errorf("after retry PrepareRoot, %q still has untrusted ACL: %+v", repo, cs)
		}
		r := checkEntriesOK(t, root, prep)
		checkQuarantined(t, r, repo, "- name: x")
	})

	t.Run("non-directory subdir fails before root repair propagates", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		mkdir(t, root)
		setDescriptor(t, root, "D:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;BU)")
		conf := filepath.Join(root, "googet.conf")
		writeFile(t, conf, "x")
		cacheFile := filepath.Join(root, "cache")
		writeFile(t, cacheFile, "not a directory")

		if _, err := PrepareRoot(root, testSubdirs); err == nil || !strings.Contains(err.Error(), "isn't a directory") {
			t.Fatalf("PrepareRoot(%q) with file at cache = %v, want non-directory error", root, err)
		}
		// repairRoot must not have run, so conf still retains its untrusted
		// inherited ACE and will be recorded on retry once cache is removed.
		if cs, _ := descriptor(t, conf); len(aclReasons(cs, false, trustedOwner(cs.OwnerSID), trustedOwner)) == 0 {
			t.Fatalf("PrepareRoot cleared untrusted inherited ACE on %q before failing on non-directory subdir: %+v", conf, cs)
		}

		if err := os.Remove(cacheFile); err != nil {
			t.Fatal(err)
		}
		prep := prepareRoot(t, root)
		r := checkEntriesOK(t, root, prep)
		checkQuarantined(t, r, conf, "x")
	})
}
