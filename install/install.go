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

// Package install handles the installation of packages.
package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/download"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/progress"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

// minInstalled reports whether the package is installed at the given version or greater.
func minInstalled(pi goolib.PackageInfo, db *googetdb.GooDB) (bool, error) {
	p, err := db.FetchPkg(pi)
	if err != nil {
		return false, err
	}
	if p.PackageSpec == nil {
		return false, nil
	}
	if p.PackageSpec.Name == pi.Name && (pi.Arch == "" || p.PackageSpec.Arch == pi.Arch) {
		c, err := goolib.Compare(pi.Ver, p.PackageSpec.Version)
		if err != nil {
			return false, err
		}
		return c < 1, nil
	}
	return false, nil
}

// isSatisfied reports whether the package dependency is satisfied by an installed package
// or a package that provides it.
func isSatisfied(pi goolib.PackageInfo, db *googetdb.GooDB) (bool, error) {
	// First check if the package itself is installed.
	ok, err := minInstalled(pi, db)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}

	// Check if any installed package provides this dependency.
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		return false, err
	}
	for _, p := range pkgs {
		if p.PackageSpec == nil {
			continue
		}
		for _, prov := range p.PackageSpec.Provides {
			if client.SatisfiesProvider(prov, pi.Name, pi.Ver) {
				return true, nil
			}
		}
	}
	return false, nil
}

func resolveConflicts(ps *goolib.PkgSpec, db *googetdb.GooDB) error {
	// Check for any conflicting packages.
	// TODO(ajackura): Make sure no conflicting packages are listed as
	// dependencies or subdependancies.
	for _, pkg := range ps.Conflicts {
		pi := goolib.PkgNameSplit(pkg)
		// Check if the conflicting package or a provider of it is installed.
		sat, err := isSatisfied(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, db)
		if err != nil {
			return err
		}
		if sat {
			return fmt.Errorf("cannot install, conflict with installed package or provider: %s", pi)
		}
	}
	return nil
}

func resolveReplacements(ctx context.Context, ps *goolib.PkgSpec, dbOnly bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	// Check for and remove any package this replaces.
	// TODO(ajackura): Make sure no replacements are listed as
	// dependencies or subdependancies.
	for _, pkg := range ps.Replaces {
		pi := goolib.PkgNameSplit(pkg)
		ins, err := minInstalled(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, db)
		if err != nil {
			return err
		}
		if !ins {
			continue
		}
		deps, _, err := remove.EnumerateDeps(pi, db)
		if err != nil {
			return err
		}
		logger.Infof("%s replaces %s, removing", ps, pi)
		if err := remove.All(ctx, pi, deps, dbOnly, downloader, db); err != nil {
			return err
		}
	}
	return nil
}

func installDeps(ctx context.Context, ps *goolib.PkgSpec, cache string, rm client.RepoMap, archs []string, dbOnly, force bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	logger.Infof("Resolving conflicts and dependencies for %s %s version %s", ps.Arch, ps.Name, ps.Version)
	if err := resolveConflicts(ps, db); err != nil {
		return err
	}
	// Check for and install any dependencies.
	for p, ver := range ps.PkgDependencies {
		pi := goolib.PkgNameSplit(p)
		ok, err := isSatisfied(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: ver}, db)
		if err != nil {
			return err
		} else if ok {
			logger.Infof("Dependency met: %s.%s with version greater than %s installed or provided", pi.Name, pi.Arch, ver)
			continue
		}

		installedArch, isLocked, _, _, err := db.InstalledLockState(pi.Name)
		if err != nil {
			logger.Infof("Error fetching installed package state: %v, proceeding without lock", err)
		}
		spec, repo, _, err := client.FindRepoLatest(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: ver}, rm, archs, installedArch, isLocked)
		if err != nil {
			return err
		}

		logger.Infof("Dependency found: %s.%s %s (provides %s) is available", spec.Name, spec.Arch, spec.Version, pi.Name)
		if err := FromRepo(ctx, goolib.PackageInfo{Name: spec.Name, Arch: spec.Arch, Ver: spec.Version}, repo, cache, rm, archs, dbOnly, force, downloader, db); err != nil {
			return err
		}
	}
	return resolveReplacements(ctx, ps, dbOnly, downloader, db)
}

// FromRepo installs a package and all dependencies from a repository.
func FromRepo(ctx context.Context, pi goolib.PackageInfo, repo, cache string, rm client.RepoMap, archs []string, dbOnly, force bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	logger.Infof("Starting install of %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Installing %s.%s.%s and dependencies...\n", pi.Name, pi.Arch, pi.Ver)
	// When a specific version is requested, look for an exact match in the repository.
	// Virtual package resolution is not currently supported for specific versions.
	// If no version is specified, resolve the latest version handling both
	// direct matches and providers.
	if pi.Ver == "" {
		installedArch, isLocked, _, _, err := db.InstalledLockState(pi.Name)
		if err != nil {
			logger.Infof("Error fetching installed package state: %v, proceeding without lock", err)
		}
		spec, repoURL, _, err := client.FindRepoLatest(pi, rm, archs, installedArch, isLocked)
		if err != nil {
			return err
		}
		repo = repoURL
		// Use the resolved real package name and version.
		pi.Name = spec.Name
		pi.Arch = spec.Arch
		pi.Ver = spec.Version
	}

	rs, err := client.FindRepoSpec(pi, rm[repo])
	if err != nil {
		return err
	}
	if err := installDeps(ctx, rs.PackageSpec, cache, rm, archs, dbOnly, force, downloader, db); err != nil {
		return err
	}

	dst, pkgURL, err := download.FromRepo(ctx, rs, repo, cache, downloader)
	if err != nil {
		return err
	}

	// The state of the installed version, if any, is read before the new
	// version replaces it in the database.
	old, err := db.FetchPkg(pi)
	if err != nil {
		return err
	}
	insFiles, err := installPkg(dst, rs.PackageSpec, dbOnly, force, db, func(insFiles map[string]string) error {
		return addPkg(db, client.PackageState{
			SourceRepo:     repo,
			DownloadURL:    pkgURL,
			Checksum:       rs.Checksum,
			LocalPath:      dst,
			PackageSpec:    rs.PackageSpec,
			InstalledFiles: insFiles,
		})
	})
	if err != nil {
		return err
	}

	logger.Infof("Installation of %s.%s.%s completed", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Installation of %s.%s.%s and all dependencies completed\n", pi.Name, pi.Arch, pi.Ver)
	// The old version is cleaned up only once the new one is recorded.
	cleanOld(old, insFiles, dbOnly)
	return nil
}

// FromDisk installs a local .goo file.
func FromDisk(pkgPath, cache string, dbOnly, force, shouldReinstall bool, db *googetdb.GooDB) error {
	if _, err := oswrap.Stat(pkgPath); err != nil {
		return err
	}
	zs, err := extractSpec(pkgPath)
	if err != nil {
		return fmt.Errorf("error extracting spec file: %v", err)
	}
	if !shouldReinstall {
		ni, err := NeedsInstallation(goolib.PackageInfo{Name: zs.Name, Arch: zs.Arch, Ver: zs.Version}, db)
		if err != nil {
			return err
		}
		if !ni {
			fmt.Printf("%s.%s.%s or a newer version is already installed on the system\n", zs.Name, zs.Arch, zs.Version)
			return nil
		}
	}

	logger.Infof("Starting install of %q, version %q from %q", zs.Name, zs.Version, pkgPath)
	fmt.Printf("Installing %s %s...\n", zs.Name, zs.Version)

	if err := resolveConflicts(zs, db); err != nil {
		return err
	}
	for p, ver := range zs.PkgDependencies {
		pi := goolib.PkgNameSplit(p)
		if ok, err := isSatisfied(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: ver}, db); err != nil {
			return err
		} else if ok {
			logger.Infof("Dependency met: %s.%s with version greater than %s installed or provided", pi.Name, pi.Arch, ver)
			continue
		}
		return fmt.Errorf("package dependency %s %s (min version %s) not installed and not provided", pi.Name, pi.Arch, ver)
	}
	for _, pkg := range zs.Replaces {
		pi := goolib.PkgNameSplit(pkg)
		ins, err := minInstalled(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, db)
		if err != nil {
			return err
		}
		if ins {
			return fmt.Errorf("cannot install, replaces installed package, remove first then try installation again: %s", pi)
		}
	}

	dst := filepath.Join(cache, goolib.PackageInfo{Name: zs.Name, Arch: zs.Arch, Ver: zs.Version}.PkgName())
	if err := copyPkg(pkgPath, dst); err != nil {
		return err
	}

	if _, err := installPkg(dst, zs, dbOnly, force, db, func(insFiles map[string]string) error {
		return addPkg(db, client.PackageState{
			LocalPath:      dst,
			PackageSpec:    zs,
			InstalledFiles: insFiles,
		})
	}); err != nil {
		return err
	}

	if shouldReinstall {
		logger.Infof("Reinstallation of %q, version %q completed", zs.Name, zs.Version)
		fmt.Printf("Reinstallation of %s completed\n", zs.Name)
	} else {
		logger.Infof("Installation of %q, version %q completed", zs.Name, zs.Version)
		fmt.Printf("Installation of %s completed\n", zs.Name)
	}
	return nil
}

// Reinstall reinstalls and optionally redownloads, a package.
func Reinstall(ctx context.Context, ps client.PackageState, rd, force bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	spec := ps.PackageSpec
	logger.Infof("Starting reinstall of %s.%s, version %s", spec.Name, spec.Arch, spec.Version)
	fmt.Printf("Reinstalling %s.%s %s and dependencies...\n", spec.Name, spec.Arch, spec.Version)

	// Fix for package install by older versions of GooGet.
	if ps.LocalPath == "" && ps.UnpackDir != "" {
		ps.LocalPath = ps.UnpackDir + ".goo"
	}

	if ps.LocalPath == "" {
		return fmt.Errorf("local path not referenced in state file for %s.%s.%s. Cannot redownload", spec.Name, spec.Arch, spec.Version)
	}

	f, err := os.Open(ps.LocalPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		logger.Infof("Local package does not exist for %s.%s.%s, redownloading...", spec.Name, spec.Arch, spec.Version)
		rd = true
	}
	// Force redownload if checksum does not match.
	// If checksum is empty this was a local install so ignore.
	if !rd && ps.Checksum != "" && goolib.Checksum(f) != ps.Checksum {
		logger.Info("Local package checksum does not match, redownloading...")
		rd = true
	}
	f.Close()

	if rd {
		if ps.DownloadURL == "" {
			return fmt.Errorf("can not redownload %s.%s.%s, DownloadURL not saved", spec.Name, spec.Arch, spec.Version)
		}
		if err := download.Package(ctx, ps.DownloadURL, ps.LocalPath, ps.Checksum, downloader); err != nil {
			return fmt.Errorf("error redownloading package: %v", err)
		}
	}

	if _, err := installPkg(ps.LocalPath, ps.PackageSpec, false, force, db, nil); err != nil {
		return fmt.Errorf("error reinstalling package: %v", err)
	}

	logger.Infof("Reinstallation of %s.%s, version %s completed", spec.Name, spec.Arch, spec.Version)
	fmt.Printf("Reinstallation of %s.%s %s completed\n", spec.Name, spec.Arch, spec.Version)
	return nil
}

func copyPkg(src, dst string) (retErr error) {
	r, err := oswrap.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()

	f, err := oswrap.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()

	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return retErr
}

// NeedsInstallation checks if a package version needs installation.
func NeedsInstallation(pi goolib.PackageInfo, db *googetdb.GooDB) (bool, error) {
	p, err := db.FetchPkg(pi)
	if err != nil {
		return true, err
	}
	if p.PackageSpec == nil {
		return true, nil
	}
	if p.PackageSpec.Name == pi.Name && p.PackageSpec.Arch == pi.Arch {
		c, err := goolib.Compare(p.PackageSpec.Version, pi.Ver)
		if err != nil {
			return true, err
		}
		switch c {
		case 0:
			logger.Infof("%s.%s %s is already installed.\n", pi.Name, pi.Arch, pi.Ver)
			return false, nil
		case 1:
			logger.Infof("A newer version of %s.%s is already installed.\n", pi.Name, pi.Arch)
			return false, nil
		}
	}
	return true, nil
}

func extractSpec(pkgPath string) (*goolib.PkgSpec, error) {
	f, err := oswrap.Open(pkgPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return goolib.ExtractPkgSpec(f)
}

func makeInstallFunction(src, dst string, insFiles map[string]string, dbOnly, force bool, conflictMap map[string]string, u *undo) func(string, os.FileInfo, error) error {
	return func(path string, fi os.FileInfo, err error) (outerr error) {
		if err != nil {
			return err
		}
		outPath := filepath.Join(dst, strings.TrimPrefix(path, src))

		if owner, ok := conflictMap[outPath]; ok && !fi.IsDir() {
			if settings.StrictConflicts && !force {
				return fmt.Errorf("file conflict: %s is already owned by package %s", outPath, owner)
			}
			if force {
				logger.Infof("Warning: file conflict: %s is already owned by package %s, overwriting due to force flag", outPath, owner)
			} else {
				logger.Infof("Warning: file conflict: %s is already owned by package %s, overwriting because `StrictConflicts` is not set", outPath, owner)
			}
			progress.Printf("Warning: file conflict: %s is already owned by package %s, overwriting...\n", outPath, owner)
		}

		if dbOnly {
			if !fi.IsDir() {
				f, err := oswrap.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				insFiles[outPath] = goolib.Checksum(f)
			}
			insFiles[outPath] = ""
			return nil
		}
		if fi.IsDir() {
			logger.Infof("Creating folder %q", outPath)
			// We designate directories by an empty hash.
			insFiles[outPath] = ""
			return u.mkdirAll(outPath, fi.Mode())
		}
		if err := u.mkdirAll(filepath.Dir(outPath), dirPerm); err != nil {
			return err
		}
		if err := u.prepare(outPath); err != nil {
			return err
		}
		logger.Infof("Copying file %q", outPath)
		oFile, err := oswrap.Create(outPath)
		if err != nil {
			return err
		}
		defer func() {
			if err := oFile.Close(); err != nil && outerr == nil {
				outerr = err
			}
		}()
		iFile, err := oswrap.Open(path)
		if err != nil {
			return err
		}
		defer iFile.Close()

		hash := sha256.New()
		mw := io.MultiWriter(oFile, hash)
		if _, err := io.Copy(mw, iFile); err != nil {
			return err
		}
		insFiles[outPath] = hex.EncodeToString(hash.Sum(nil))
		return nil
	}
}

func resolveDst(dst string) string {
	if !filepath.IsAbs(dst) {
		if strings.HasPrefix(dst, "<") {
			if i := strings.LastIndex(dst, ">"); i != -1 {
				return os.Getenv(dst[1:i]) + dst[i+1:]
			}
		}
		return "/" + dst
	}
	return dst
}

func cleanOld(st client.PackageState, insFiles map[string]string, dbOnly bool) {
	if !dbOnly {
		cleanOldFiles(st, insFiles)
	}
	if st.LocalPath != "" && oswrap.RemoveAll(st.LocalPath) != nil {
		logger.Error("Unable to remove old local path")
	}
	if st.UnpackDir != "" && oswrap.RemoveAll(st.UnpackDir) != nil {
		logger.Error("Unable to remove old unpack dir")
	}
}

func cleanOldFiles(oldState client.PackageState, insFiles map[string]string) {
	if len(oldState.InstalledFiles) == 0 {
		return
	}
	var files []string
	for file := range oldState.InstalledFiles {
		if chksum, ok := insFiles[file]; !ok {
			if chksum == "" {
				files = append(files, file)
				continue
			}
			logger.Infof("Cleaning up old file %q", file)
			if _, err := client.RemoveOrRename(file); err != nil {
				logger.Error(err)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	for _, dir := range files {
		if _, err := client.RemoveOrRename(dir); err != nil {
			logger.Info(err)
		}
	}
}

func buildConflictMap(db *googetdb.GooDB, currentPkg string) (map[string]string, error) {
	conflictMap := make(map[string]string)
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		return nil, err
	}
	for _, p := range pkgs {
		if p.PackageSpec == nil || p.PackageSpec.Name == currentPkg {
			continue
		}
		for f, hash := range p.InstalledFiles {
			if hash != "" {
				conflictMap[f] = p.PackageSpec.Name
			}
		}
	}
	return conflictMap, nil
}

// errInstallInterrupted is the spinner status error used when an install
// unwinds without returning, such as on a panic.
var errInstallInterrupted = errors.New("install interrupted")

// addPkg records an installed package in the database. Tests replace it to
// make the update fail.
var addPkg = (*googetdb.GooDB).AddPkg

// installPkg extracts and installs a package, rendering a spinner on
// interactive terminals for the duration of the install.
//
// If record is not nil, it must persist the install, such as by recording it
// in the database. It is called with the installed files after the files are
// placed and the installer has succeeded. If it fails or panics, the placed
// files are rolled back, but changes made by the installer are not. The
// backups of overwritten files are kept until record succeeds.
func installPkg(pkg string, ps *goolib.PkgSpec, dbOnly, force bool, db *googetdb.GooDB, record func(insFiles map[string]string) error) (insFiles map[string]string, err error) {
	sp := progress.NewSpinner(fmt.Sprintf("Installing %s.%s.%s", ps.Name, ps.Arch, ps.Version))
	// The spinner is stopped by a deferred call so that no exit path leaves it
	// redrawing. A normal return overwrites err before the deferred call runs;
	// a panic leaves errInstallInterrupted in place, so it renders "failed".
	err = errInstallInterrupted
	defer func() { sp.Stop(err) }()
	return installPkgInner(pkg, ps, dbOnly, force, db, record)
}

// backupInfix marks the backup of a file that an install overwrites.
const backupInfix = ".googet-bak"

// change is one filesystem change made by an install.
type change struct {
	path string
	// backup is the path to the backup of path's old content, or empty if
	// path was created.
	backup string
}

// undo records the changes an install makes to the filesystem, in order, so
// that they can be reverted if the install fails. Reverting them newest first
// restores each path to its original state even if it was written more than
// once. The zero value is ready to use.
type undo struct {
	changes []change
}

// dirPerm is the mode of the parent directories an install creates. It
// matches download.ExtractPkg.
const dirPerm os.FileMode = 0755

// mkdirAll creates dir with the given mode and any missing parents with mode
// dirPerm, recording each directory it creates. It creates only the missing
// directories, so it never calls oswrap.MkdirAll on an existing path such as
// a Windows volume root, which panics.
func (u *undo) mkdirAll(dir string, mode os.FileMode) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := oswrap.Lstat(d); !os.IsNotExist(err) || filepath.Dir(d) == d {
			break
		}
		missing = append(missing, d)
	}
	if len(missing) == 0 {
		fi, err := oswrap.Stat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	}
	for i := len(missing) - 1; i >= 0; i-- {
		u.changes = append(u.changes, change{path: missing[i]})
	}
	if len(missing) > 1 {
		if err := oswrap.MkdirAll(missing[1], dirPerm); err != nil {
			return err
		}
	}
	return oswrap.MkdirAll(dir, mode)
}

// prepare makes path ready to be written. An existing file is renamed to a
// backup in the same directory, which works on Windows even for running
// executables; a missing path is recorded as created. If googet is killed
// before commit or rollback runs, the backup is left behind as
// name.googet-bak*.
func (u *undo) prepare(path string) error {
	fi, err := oswrap.Lstat(path)
	switch {
	case os.IsNotExist(err):
		u.changes = append(u.changes, change{path: path})
		return nil
	case err != nil:
		return err
	case fi.IsDir():
		// An empty directory in the way of the file is removed, and is not
		// recreated on rollback; client.RemoveOrRename fails if it is not
		// empty.
		if _, err := client.RemoveOrRename(path); err != nil {
			return err
		}
		u.changes = append(u.changes, change{path: path})
		return nil
	}
	// oswrap has no CreateTemp; os handles long absolute Windows paths itself.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+backupInfix+"*")
	if err != nil {
		return err
	}
	f.Close()
	if err := oswrap.Rename(path, f.Name()); err != nil {
		oswrap.Remove(f.Name())
		return err
	}
	u.changes = append(u.changes, change{path: path, backup: f.Name()})
	return nil
}

// rollback reverts the changes, newest first: created paths are removed and
// backups are restored. Directories that are not empty are left in place.
func (u *undo) rollback() {
	for i := len(u.changes) - 1; i >= 0; i-- {
		c := u.changes[i]
		// RemoveOrRename moves a file that is in use out of the way. If it
		// returns a name, the file was moved there, even if scheduling its
		// removal on reboot then failed.
		fn, err := client.RemoveOrRename(c.path)
		switch {
		case err != nil && fn == "":
			logger.Warningf("Rollback: leaving %q: %v", c.path, err)
			continue
		case err != nil:
			logger.Warningf("Rollback: moved %q to %q, but scheduling its removal on reboot failed: %v", c.path, fn, err)
		}
		if c.backup == "" {
			continue
		}
		if err := oswrap.Rename(c.backup, c.path); err != nil {
			logger.Errorf("Rollback: failed to restore %q from %q: %v", c.path, c.backup, err)
		}
	}
}

// commit deletes the backups, scheduling any that are in use for removal on
// reboot.
func (u *undo) commit() {
	for _, c := range u.changes {
		if c.backup == "" {
			continue
		}
		if err := oswrap.Remove(c.backup); err != nil {
			// RemoveOnReboot does nothing off Windows, so log the failure.
			logger.Warningf("Failed to remove backup %q, falling back to removal on reboot (Windows only): %v", c.backup, err)
			if err := oswrap.RemoveOnReboot(c.backup); err != nil {
				logger.Errorf("Failed to schedule removal of backup %q on reboot: %v", c.backup, err)
			}
		}
	}
}

// installPkgInner implements installPkg. It owns the journal of the
// filesystem changes: if any step, including record, fails or panics, the
// files it placed are removed and the files it overwrote are restored.
func installPkgInner(pkg string, ps *goolib.PkgSpec, dbOnly, force bool, db *googetdb.GooDB, record func(insFiles map[string]string) error) (map[string]string, error) {
	dir, err := download.ExtractPkg(pkg)
	if err != nil {
		return nil, err
	}

	logger.Infof("Executing install of package %q", filepath.Base(dir))

	u := &undo{}
	// done is set only on the final return so that errors and panics both
	// roll back.
	done := false
	defer func() {
		if done {
			u.commit()
		} else {
			u.rollback()
		}
	}()

	conflictMap, err := buildConflictMap(db, ps.Name)
	if err != nil {
		return nil, err
	}

	insFiles := make(map[string]string)
	for src, dst := range ps.Files {
		dst = resolveDst(dst)
		src = filepath.Join(dir, src)
		if err := oswrap.Walk(src, makeInstallFunction(src, dst, insFiles, dbOnly, force, conflictMap, u)); err != nil {
			return nil, err
		}
	}

	if !dbOnly {
		if err := system.Install(dir, ps); err != nil {
			return nil, err
		}
	}

	if record != nil {
		if err := record(insFiles); err != nil {
			if !dbOnly && ps.Install.Path != "" {
				logger.Errorf("Recording %s.%s.%s in the database failed after its installer ran; rolling back its files, but not the changes made by the installer: %v", ps.Name, ps.Arch, ps.Version, err)
			} else {
				logger.Errorf("Recording %s.%s.%s in the database failed: %v", ps.Name, ps.Arch, ps.Version, err)
			}
			return nil, err
		}
	}

	done = true
	if err := oswrap.RemoveAll(dir); err != nil {
		logger.Error(err)
	}

	return insFiles, nil
}

func listDeps(pi goolib.PackageInfo, rm client.RepoMap, repo string, dl []goolib.PackageInfo, archs []string, db *googetdb.GooDB) ([]goolib.PackageInfo, error) {
	rs, err := client.FindRepoSpec(pi, rm[repo])
	if err != nil {
		return nil, err
	}
	dl = append(dl, pi)
	for d, v := range rs.PackageSpec.PkgDependencies {
		di := goolib.PkgNameSplit(d)
		installedArch, isLocked, _, _, err := db.InstalledLockState(di.Name)
		if err != nil {
			logger.Infof("Error fetching installed package state: %v, proceeding without lock", err)
		}
		spec, repo, arch, err := client.FindRepoLatest(di, rm, archs, installedArch, isLocked)
		di.Arch = arch
		if err != nil {
			return nil, fmt.Errorf("cannot resolve dependency %s.%s.%s: %v", di.Name, di.Arch, di.Ver, err)
		}
		c, err := goolib.Compare(spec.Version, v)
		if err != nil {
			return nil, err
		}
		if c == -1 {
			return nil, fmt.Errorf("cannot resolve dependency, %s.%s version %s or greater not installed and not available in any repo", pi.Name, pi.Arch, pi.Ver)
		}
		di.Ver = spec.Version
		dl, err = listDeps(di, rm, repo, dl, archs, db)
		if err != nil {
			return nil, err
		}
	}
	return dl, nil
}

// ListDeps returns a list of dependencies and subdependancies for a package.
func ListDeps(pi goolib.PackageInfo, rm client.RepoMap, repo string, archs []string, db *googetdb.GooDB) ([]goolib.PackageInfo, error) {
	logger.Infof("Building dependency list for %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	return listDeps(pi, rm, repo, nil, archs, db)
}
