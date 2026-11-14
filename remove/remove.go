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

// Package remove handles the removal of packages.
package remove

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/download"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

// Mode selects how much of a package's removal is carried out.
type Mode int

const (
	// RunUninstaller runs the package's uninstall command, then removes its
	// files, cached package and database entry.
	RunUninstaller Mode = iota
	// SkipUninstaller removes the package's files, cached package, googet's
	// "Add or remove programs" entry and database entry without running its
	// uninstall command, so the package is never read or downloaded.
	SkipUninstaller
	// DBOnly only removes the database entry. It doesn't run the uninstall
	// command, and leaves the files and the cached package in place.
	DBOnly
)

// valid reports whether m is one of the defined modes.
func (m Mode) valid() bool {
	return m >= RunUninstaller && m <= DBOnly
}

// String returns the mode's name, or Mode(N) for a value outside the enum.
func (m Mode) String() string {
	switch m {
	case RunUninstaller:
		return "RunUninstaller"
	case SkipUninstaller:
		return "SkipUninstaller"
	case DBOnly:
		return "DBOnly"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// ErrPackageUnavailable is returned when a package's uninstall command needs
// the package itself, and the package is neither in the cache nor available
// for download.
var ErrPackageUnavailable = errors.New("package needed by its uninstall command is not available")

// runUninstall runs the uninstall command for ps, if it has one. The package
// is only read from the cache, or downloaded again, when the command needs
// it.
func runUninstall(ctx context.Context, ps *client.PackageState, downloader *client.Downloader) error {
	name := ps.PackageSpec.String()
	if !system.UninstallNeedsPackage(ps.PackageSpec) {
		// The command doesn't use the package, but system.Uninstall still
		// needs a directory for its log.
		dir, err := os.MkdirTemp("", "googet-remove-")
		if err != nil {
			return err
		}
		defer func() {
			if err := oswrap.RemoveAll(dir); err != nil {
				logger.Error(err)
			}
		}()
		return system.Uninstall(dir, ps)
	}

	if ps.LocalPath == "" {
		return fmt.Errorf("%w: no local path saved for %s", ErrPackageUnavailable, name)
	}
	f, err := os.Open(ps.LocalPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var rd bool
	if os.IsNotExist(err) {
		logger.Infof("Local package does not exist for %s, redownloading...", name)
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
			return fmt.Errorf("%w: %s is not cached and its DownloadURL was not saved", ErrPackageUnavailable, name)
		}
		if err := download.Package(ctx, ps.DownloadURL, ps.LocalPath, ps.Checksum, downloader); err != nil {
			return fmt.Errorf("%w: redownloading %s failed, it may no longer exist in the repo: %w", ErrPackageUnavailable, name, err)
		}
	}

	// Remove the extraction directory however this returns, including when
	// the extraction itself fails part way. A cached path with no extension
	// would name the package itself, which must be kept on failure.
	if dir := download.ExtractDir(ps.LocalPath); dir != ps.LocalPath {
		defer func() {
			if err := oswrap.RemoveAll(dir); err != nil {
				logger.Error(err)
			}
		}()
	}
	eDir, err := download.ExtractPkg(ps.LocalPath)
	if err != nil {
		return err
	}
	return system.Uninstall(eDir, ps)
}

// removeInstalledFiles removes the files and then the directories a package
// installed. Directories are removed deepest first, and only once empty.
func removeInstalledFiles(files map[string]string) {
	var dirs []string
	for file, chksum := range files {
		if chksum == "" {
			dirs = append(dirs, file)
			continue
		}
		logger.Infof("Removing %q", file)
		if _, err := client.RemoveOrRename(file); err != nil {
			logger.Error(err)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		logger.Infof("Removing %q", dir)
		if _, err := client.RemoveOrRename(dir); err != nil {
			logger.Info(err)
		}
	}
}

// uninstallPkg removes a package as mode describes.
func uninstallPkg(ctx context.Context, pi goolib.PackageInfo, mode Mode, downloader *client.Downloader, db *googetdb.GooDB) error {
	if !mode.valid() {
		return fmt.Errorf("invalid removal mode %v", mode)
	}
	logger.Infof("Executing removal of package %q (%v)", pi.Name, mode)
	ps, err := db.FetchPkg(pi)
	if err != nil {
		return fmt.Errorf("package not found in state file: %v", err)
	}
	if ps.PackageSpec == nil {
		return fmt.Errorf("package %s.%s is not installed", pi.Name, pi.Arch)
	}

	if mode != DBOnly {
		// Fix for package install by older versions of GooGet.
		if ps.LocalPath == "" && ps.UnpackDir != "" {
			ps.LocalPath = ps.UnpackDir + ".goo"
		}

		if mode == SkipUninstaller {
			logger.Infof("Not running the uninstall command for %s", ps.PackageSpec)
			// The uninstall command would have removed this entry.
			if err := system.RemoveUninstallEntry(ps.PackageSpec.Name); err != nil {
				logger.Error(err)
			}
		} else if err := runUninstall(ctx, &ps, downloader); err != nil {
			return err
		}

		removeInstalledFiles(ps.InstalledFiles)
		if ps.LocalPath != "" {
			if err := oswrap.RemoveAll(ps.LocalPath); err != nil {
				logger.Errorf("error removing package data from cache directory: %v", err)
			}
		}
	}
	return db.RemovePkg(ps.PackageSpec.Name, ps.PackageSpec.Arch)
}

// DepMap is a map of packages to dependant packages.
type DepMap map[string][]string

func (deps DepMap) remove(name string) {
	for dep, s := range deps {
		for i, d := range s {
			if d == name {
				s[i] = s[len(s)-1]
				s = s[:len(s)-1]
				deps[dep] = s
				break
			}
		}
	}
	delete(deps, name)
}

func (deps DepMap) build(name, arch string, state client.GooGetState) {
	logger.Infof("Building dependency map for %q", name)
	deps[name+"."+arch] = nil
	for _, p := range state {
		if p.PackageSpec.Name == name && p.PackageSpec.Arch == arch {
			continue
		}
		for d := range p.PackageSpec.PkgDependencies {
			di := goolib.PkgNameSplit(d)
			if di.Name == name && (di.Arch == arch || di.Arch == "") {
				n, a := p.PackageSpec.Name, p.PackageSpec.Arch
				deps[name+"."+arch] = append(deps[name+"."+arch], n+"."+a)
				deps.build(n, a, state)
			}
		}
	}
}

// EnumerateDeps returns a DepMap and list of dependencies for a package.
func EnumerateDeps(pi goolib.PackageInfo, db *googetdb.GooDB) (DepMap, []string, error) {
	dm := make(DepMap)
	state, err := db.FetchPkgs("")
	if err != nil {
		return nil, nil, fmt.Errorf("Error connecting to state database while building dependency map: %v", err)
	}
	dm.build(pi.Name, pi.Arch, state)
	var dl []string
	for k := range dm {
		di := goolib.PkgNameSplit(k)
		ps, err := state.GetPackageState(di)
		if err != nil {
			return nil, nil, fmt.Errorf("error finding package in state file, even though the dependancy map was just built: %v", err)
		}
		dl = append(dl, k+" "+ps.PackageSpec.Version)
	}
	return dm, dl, nil
}

// All removes a package and all dependent packages, each as mode describes.
// Packages with no dependent packages will be removed first.
func All(ctx context.Context, pi goolib.PackageInfo, deps DepMap, mode Mode, downloader *client.Downloader, db *googetdb.GooDB) error {
	if !mode.valid() {
		return fmt.Errorf("invalid removal mode %v", mode)
	}
	// deps includes an entry for pi itself, but we don't need to iterate over it
	// because pi is always uninstalled at the end.
	delete(deps, pi.Name+"."+pi.Arch)
	for len(deps) > 0 {
		for dep := range deps {
			if len(deps[dep]) == 0 {
				di := goolib.PkgNameSplit(dep)
				if err := uninstallPkg(ctx, di, mode, downloader, db); err != nil {
					return err
				}
				deps.remove(dep)
			}
		}
	}
	return uninstallPkg(ctx, pi, mode, downloader, db)
}
