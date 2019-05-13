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
	"fmt"
	"os"
	"sort"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/download"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

func uninstallPkg(ctx context.Context, pi goolib.PackageInfo, state *client.GooGetState, dbOnly bool, proxyServer string) error {
	logger.Infof("Executing removal of package %q", pi.Name)
	ps, err := state.GetPackageState(pi)
	if err != nil {
		return fmt.Errorf("package not found in state file: %v", err)
	}

	if !dbOnly {
		// Fix for package install by older versions of GooGet.
		if ps.LocalPath == "" && ps.UnpackDir != "" {
			ps.LocalPath = ps.UnpackDir + ".goo"
		}
		if ps.LocalPath == "" {
			return fmt.Errorf("no local path available for package %q", pi.Name)
		}

		f, err := os.Open(ps.LocalPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		var rd bool
		if os.IsNotExist(err) {
			logger.Infof("Local package does not exist for %s.%s.%s, redownloading...", pi.Name, pi.Arch, pi.Ver)
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
				return fmt.Errorf("can not redownload %s.%s.%s, DownloadURL not saved", pi.Name, pi.Arch, pi.Ver)
			}
			if err := download.Package(ctx, ps.DownloadURL, ps.LocalPath, ps.Checksum, proxyServer); err != nil {
				return fmt.Errorf("error redownloading %s.%s.%s, package may no longer exist in the repo, you can use the '-db_only' flag to remove it form the database: %v", pi.Name, pi.Arch, pi.Ver, err)
			}
		}

		eDir, err := download.ExtractPkg(ps.LocalPath)
		if err != nil {
			return err
		}

		if err := system.Uninstall(eDir, ps.PackageSpec); err != nil {
			return err
		}

		if err := oswrap.RemoveAll(eDir); err != nil {
			logger.Error(err)
		}

		if len(ps.InstalledFiles) > 0 {
			var dirs []string
			for file, chksum := range ps.InstalledFiles {
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
		if err := oswrap.RemoveAll(ps.LocalPath); err != nil {
			logger.Errorf("error removing package data from cache directory: %v", err)
		}
	}
	return state.Remove(pi)
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
func EnumerateDeps(pi goolib.PackageInfo, state client.GooGetState) (DepMap, []string) {
	dm := make(DepMap)
	dm.build(pi.Name, pi.Arch, state)
	var dl []string
	for k := range dm {
		di := goolib.PkgNameSplit(k)
		ps, err := state.GetPackageState(di)
		if err != nil {
			logger.Fatalf("error finding package in state file, even though the dependancy map was just built: %v", err)
		}
		dl = append(dl, k+" "+ps.PackageSpec.Version)
	}
	return dm, dl
}

// All removes a package and all dependant packages. Packages with no dependant packages
// will be removed first.
func All(ctx context.Context, pi goolib.PackageInfo, deps DepMap, state *client.GooGetState, dbOnly bool, proxyServer string) error {
	for len(deps) > 1 {
		for dep := range deps {
			if len(deps[dep]) == 0 {
				di := goolib.PkgNameSplit(dep)
				if err := uninstallPkg(ctx, di, state, dbOnly, proxyServer); err != nil {
					return err
				}
				deps.remove(dep)
			}
		}
	}
	return uninstallPkg(ctx, pi, state, dbOnly, proxyServer)
}
