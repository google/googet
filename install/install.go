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
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

var toRemove []string

// minInstalled reports whether the package is installed at the given version or greater.
func minInstalled(pi goolib.PackageInfo, db *googetdb.GooDB) (bool, error) {
	p, err := db.FetchPkg(pi.Name)
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
			pName := prov
			pVer := ""
			// We support "name=version" format for providers.
			if i := strings.Index(prov, "="); i != -1 {
				pName = prov[:i]
				pVer = prov[i+1:]
			}

			if pName == pi.Name {
				// If we just need the name, or if the provider has no version (implies all versions),
				// or if the provider version satisfies the requirement.
				if pi.Ver == "" || pVer == "" {
					return true, nil
				}
				c, err := goolib.Compare(pVer, pi.Ver)
				if err != nil {
					logger.Errorf("Error comparing versions for provider %s: %v", prov, err)
					continue
				}
				if c > -1 {
					return true, nil
				}
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

func installDeps(ctx context.Context, ps *goolib.PkgSpec, cache string, rm client.RepoMap, archs []string, dbOnly bool, downloader *client.Downloader, db *googetdb.GooDB) error {
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

		spec, repo, err := client.FindSatisfyingRepoLatest(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: ver}, rm, archs)
		if err != nil {
			return err
		}

		logger.Infof("Dependency found: %s.%s %s (provides %s) is available", spec.Name, spec.Arch, spec.Version, pi.Name)
		if err := FromRepo(ctx, goolib.PackageInfo{Name: spec.Name, Arch: spec.Arch, Ver: spec.Version}, repo, cache, rm, archs, dbOnly, downloader, db); err != nil {
			return err
		}
	}
	return resolveReplacements(ctx, ps, dbOnly, downloader, db)
}

// FromRepo installs a package and all dependencies from a repository.
func FromRepo(ctx context.Context, pi goolib.PackageInfo, repo, cache string, rm client.RepoMap, archs []string, dbOnly bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	logger.Infof("Starting install of %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Installing %s.%s.%s and dependencies...\n", pi.Name, pi.Arch, pi.Ver)
	var rs goolib.RepoSpec
	var err error
	// If the version is empty, we need to find the latest version.
	// We also try FindSatisfyingRepoLatest to handle providers if finding by exact name fails or if we want latest compatible.
	// But FromRepo is typically called with a specific version (from installDeps or user input).
	// If called with specific version, we usually want THAT version.
	// Users might run "googet install virtual_pkg". In that case pi.Ver might be empty.
	if pi.Ver == "" {
		spec, repoURL, err := client.FindSatisfyingRepoLatest(pi, rm, archs)
		if err != nil {
			return err
		}
		repo = repoURL
		// We found a satisfying package using the new logic. Use its real name and version.
		// NOTE: This might switch the name from a virtual one to the real one.
		pi.Name = spec.Name
		pi.Arch = spec.Arch
		pi.Ver = spec.Version
		rs, err = client.FindRepoSpec(goolib.PackageInfo{Name: spec.Name, Arch: spec.Arch, Ver: spec.Version}, rm[repo])
	} else {
		// Even if name is virtual, if version is specified, we might need to resolve it?
		// But existing FindRepoSpec relies on exact name match inside the repo object.
		// If "googet install virtual_pkg.noarch.1.0.0", FindRepoSpec won't find it if it's virtual.
		// So we SHOULD use FindSatisfyingRepoLatest (or similar) here too if exact match fails?
		// For now, let's keep original behavior for explicit version invocations unless we want to support "install virtual=1.0".
		// Actually, let's try strict match first, if fail, try satisfying logic?
		// But FindRepoSpec takes a Repo struct, not RepoMap.
		// Let's stick to simple flow for now (installDeps usage).
		rs, err = client.FindRepoSpec(pi, rm[repo])
	}

	if err != nil {
		return err
	}
	if err := installDeps(ctx, rs.PackageSpec, cache, rm, archs, dbOnly, downloader, db); err != nil {
		return err
	}

	dst, pkgURL, err := download.FromRepo(ctx, rs, repo, cache, downloader)
	if err != nil {
		return err
	}

	insFiles, err := installPkg(dst, rs.PackageSpec, dbOnly)
	if err != nil {
		return err
	}

	logger.Infof("Installation of %s.%s.%s completed", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Installation of %s.%s.%s and all dependencies completed\n", pi.Name, pi.Arch, pi.Ver)
	// Clean up old version, if applicable.
	ps, err := db.FetchPkg(pi.Name)
	if err != nil {
		return err
	}
	cleanOld(ps, insFiles, dbOnly)

	return db.AddPkg(client.PackageState{
		SourceRepo:     repo,
		DownloadURL:    pkgURL,
		Checksum:       rs.Checksum,
		LocalPath:      dst,
		PackageSpec:    rs.PackageSpec,
		InstalledFiles: insFiles,
	})
}

// FromDisk installs a local .goo file.
func FromDisk(pkgPath, cache string, dbOnly, shouldReinstall bool, db *googetdb.GooDB) error {
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

	insFiles, err := installPkg(dst, zs, dbOnly)
	if err != nil {
		return err
	}

	if shouldReinstall {
		logger.Infof("Reinstallation of %q, version %q completed", zs.Name, zs.Version)
		fmt.Printf("Reinstallation of %s completed\n", zs.Name)
	} else {
		logger.Infof("Installation of %q, version %q completed", zs.Name, zs.Version)
		fmt.Printf("Installation of %s completed\n", zs.Name)
	}
	return db.AddPkg(client.PackageState{
		LocalPath:      dst,
		PackageSpec:    zs,
		InstalledFiles: insFiles,
	})
}

// Reinstall reinstalls and optionally redownloads, a package.
func Reinstall(ctx context.Context, ps client.PackageState, rd bool, downloader *client.Downloader) error {
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

	if _, err := installPkg(ps.LocalPath, ps.PackageSpec, false); err != nil {
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
	p, err := db.FetchPkg(pi.Name)
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

func makeInstallFunction(src, dst string, insFiles map[string]string, dbOnly bool) func(string, os.FileInfo, error) error {
	return func(path string, fi os.FileInfo, err error) (outerr error) {
		if err != nil {
			return err
		}
		outPath := filepath.Join(dst, strings.TrimPrefix(path, src))
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
			return oswrap.MkdirAll(outPath, fi.Mode())
		}
		fn, err := client.RemoveOrRename(outPath)
		if err != nil {
			return err
		}
		if fn != "" {
			toRemove = append(toRemove, fn)
		}
		logger.Infof("Copying file %q", outPath)
		oFile, err := oswrap.Create(outPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := oswrap.MkdirAll(filepath.Dir(outPath), fi.Mode()); err != nil {
				return err
			}
			if oFile, err = oswrap.Create(outPath); err != nil {
				return err
			}
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
	return
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

func installPkg(pkg string, ps *goolib.PkgSpec, dbOnly bool) (map[string]string, error) {
	dir, err := download.ExtractPkg(pkg)
	if err != nil {
		return nil, err
	}

	logger.Infof("Executing install of package %q", filepath.Base(dir))

	toRemove = []string{}
	// Try to cleanup moved files after package is installed.
	defer func() {
		for _, fn := range toRemove {
			oswrap.Remove(fn)
		}
	}()

	insFiles := make(map[string]string)
	for src, dst := range ps.Files {
		dst = resolveDst(dst)
		src = filepath.Join(dir, src)
		if err := oswrap.Walk(src, makeInstallFunction(src, dst, insFiles, dbOnly)); err != nil {
			return nil, err
		}
	}

	if !dbOnly {
		if err := system.Install(dir, ps); err != nil {
			return nil, err
		}
	}

	if err := oswrap.RemoveAll(dir); err != nil {
		logger.Error(err)
	}

	return insFiles, nil
}

func listDeps(pi goolib.PackageInfo, rm client.RepoMap, repo string, dl []goolib.PackageInfo, archs []string) ([]goolib.PackageInfo, error) {
	rs, err := client.FindRepoSpec(pi, rm[repo])
	if err != nil {
		return nil, err
	}
	dl = append(dl, pi)
	for d, v := range rs.PackageSpec.PkgDependencies {
		di := goolib.PkgNameSplit(d)
		ver, repo, arch, err := client.FindRepoLatest(di, rm, archs)
		di.Arch = arch
		if err != nil {
			return nil, fmt.Errorf("cannot resolve dependency %s.%s.%s: %v", di.Name, di.Arch, di.Ver, err)
		}
		c, err := goolib.Compare(ver, v)
		if err != nil {
			return nil, err
		}
		if c == -1 {
			return nil, fmt.Errorf("cannot resolve dependency, %s.%s version %s or greater not installed and not available in any repo", pi.Name, pi.Arch, pi.Ver)
		}
		di.Ver = ver
		dl, err = listDeps(di, rm, repo, dl, archs)
		if err != nil {
			return nil, err
		}
	}
	return dl, nil
}

// ListDeps returns a list of dependencies and subdependancies for a package.
func ListDeps(pi goolib.PackageInfo, rm client.RepoMap, repo string, archs []string) ([]goolib.PackageInfo, error) {
	logger.Infof("Building dependency list for %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	return listDeps(pi, rm, repo, nil, archs)
}
