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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/googet/client"
	"github.com/google/googet/download"
	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/googet/system"
	"github.com/google/logger"
)

// minInstalled reports whether the package is installed at the given version or greater.
func minInstalled(pi goolib.PackageInfo, state client.GooGetState) (bool, error) {
	for _, p := range state {
		if p.PackageSpec.Name == pi.Name && (pi.Arch == "" || p.PackageSpec.Arch == pi.Arch) {
			c, err := goolib.Compare(pi.Ver, p.PackageSpec.Version)
			if err != nil {
				return false, err
			}
			return c < 1, nil
		}
	}
	return false, nil
}

func installDeps(ps *goolib.PkgSpec, cache string, rm client.RepoMap, archs []string, state *client.GooGetState, dbOnly bool) error {
	logger.Infof("Resolving dependencies for %s %s version %s", ps.Arch, ps.Name, ps.Version)
	for p, ver := range ps.PkgDependencies {
		pi := goolib.PkgNameSplit(p)
		mi, err := minInstalled(goolib.PackageInfo{pi.Name, pi.Arch, ver}, *state)
		if err != nil {
			return err
		}
		if mi {
			logger.Infof("Dependency met: %s.%s with version greater than %s installed", pi.Name, pi.Arch, ver)
			continue
		}
		var ins bool
		v, repo, arch, err := client.FindRepoLatest(goolib.PackageInfo{pi.Name, pi.Arch, ""}, rm, archs)
		if err != nil {
			return err
		}
		c, err := goolib.Compare(v, ver)
		if err != nil {
			return err
		}
		if c > -1 {
			logger.Infof("Dependency found: %s.%s %s is available", pi.Name, arch, v)
			if err := FromRepo(goolib.PackageInfo{pi.Name, arch, v}, repo, cache, rm, archs, state, dbOnly); err != nil {
				return err
			}
			ins = true
		}
		if !ins {
			return fmt.Errorf("cannot resolve dependancy, %s.%s version %s or greater not installed and not available in any repo", pi.Name, arch, ver)
		}
	}
	return nil
}

// Latest installs the latest version of a package.
func Latest(pi goolib.PackageInfo, cache string, rm client.RepoMap, archs []string, state *client.GooGetState, dbOnly bool) error {
	ver, repo, arch, err := client.FindRepoLatest(pi, rm, archs)
	if err != nil {
		return err
	}
	return FromRepo(goolib.PackageInfo{pi.Name, arch, ver}, repo, cache, rm, archs, state, dbOnly)
}

// FromRepo installs a package and all dependencies from a repository.
func FromRepo(pi goolib.PackageInfo, repo, cache string, rm client.RepoMap, archs []string, state *client.GooGetState, dbOnly bool) error {
	ni, err := NeedsInstallation(pi, *state)
	if err != nil {
		return err
	}
	if !ni {
		return nil
	}

	logger.Infof("Starting install of %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Installing %s.%s.%s and dependencies...\n", pi.Name, pi.Arch, pi.Ver)
	rs, err := client.FindRepoSpec(pi, rm[repo])
	if err != nil {
		return err
	}
	if err := installDeps(rs.PackageSpec, cache, rm, archs, state, dbOnly); err != nil {
		return err
	}

	dst, err := download.FromRepo(rs, repo, cache)
	if err != nil {
		return err
	}

	dir, err := extractPkg(dst)
	if err != nil {
		return err
	}

	insFiles, err := installPkg(dir, rs.PackageSpec, dbOnly)
	if err != nil {
		return err
	}

	logger.Infof("Installation of %s.%s.%s completed", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Installation of %s.%s.%s and all dependencies completed\n", pi.Name, pi.Arch, pi.Ver)
	// Clean up old version, if applicable.
	pi = goolib.PackageInfo{pi.Name, pi.Arch, ""}
	if st, err := state.GetPackageState(pi); err == nil {
		if !dbOnly {
			cleanOldFiles(dir, st, insFiles)
		}
		if err := oswrap.RemoveAll(st.UnpackDir); err != nil {
			logger.Error(err)
		}
		if err := state.Remove(pi); err != nil {
			return err
		}
	}
	state.Add(client.PackageState{
		SourceRepo:     repo,
		DownloadURL:    strings.TrimSuffix(repo, filepath.Base(repo)) + rs.Source,
		Checksum:       rs.Checksum,
		UnpackDir:      dir,
		PackageSpec:    rs.PackageSpec,
		InstalledFiles: insFiles,
	})
	return nil
}

// FromDisk installs a local .goo file.
func FromDisk(arg, cache string, state *client.GooGetState, dbOnly, ri bool) error {
	if _, err := oswrap.Stat(arg); err != nil {
		return err
	}

	zs, err := extractSpec(arg)
	if err != nil {
		return fmt.Errorf("error extracting spec file: %v", err)
	}

	if !ri {
		ni, err := NeedsInstallation(goolib.PackageInfo{zs.Name, zs.Arch, zs.Version}, *state)
		if err != nil {
			return err
		}
		if !ni {
			fmt.Printf("%s.%s.%s or a newer version is already installed on the system\n", zs.Name, zs.Arch, zs.Version)
			return nil
		}
	}

	logger.Infof("Starting install of %q, version %q from %q", zs.Name, zs.Version, arg)
	fmt.Printf("Installing %s %s...\n", zs.Name, zs.Version)

	for p, ver := range zs.PkgDependencies {
		pi := goolib.PkgNameSplit(p)
		mi, err := minInstalled(goolib.PackageInfo{pi.Name, pi.Arch, ver}, *state)
		if err != nil {
			return err
		}
		if mi {
			logger.Infof("Dependency met: %s.%s with version greater than %s installed", pi.Name, pi.Arch, ver)
			continue
		}
		return fmt.Errorf("Package dependency %s %s (min version %s) not installed.\n", pi.Name, pi.Arch, ver)
	}

	dst := filepath.Join(cache, goolib.PackageInfo{zs.Name, zs.Arch, zs.Version}.PkgName())
	if err := copyPkg(arg, dst); err != nil {
		return err
	}

	dir, err := extractPkg(dst)
	if err != nil {
		return err
	}

	insFiles, err := installPkg(dir, zs, dbOnly)
	if err != nil {
		return err
	}

	if ri {
		logger.Infof("Reinstallation of %q, version %q completed", zs.Name, zs.Version)
		fmt.Printf("Reinstallation of %s completed\n", zs.Name)
		return nil
	}

	logger.Infof("Installation of %q, version %q completed", zs.Name, zs.Version)
	fmt.Printf("Installation of %s completed\n", zs.Name)

	// Clean up old version, if applicable.
	pi := goolib.PackageInfo{zs.Name, zs.Arch, ""}
	if st, err := state.GetPackageState(pi); err == nil {
		if !dbOnly {
			cleanOldFiles(dir, st, insFiles)
		}
		if err := oswrap.RemoveAll(st.UnpackDir); err != nil {
			logger.Error(err)
		}
		if err := state.Remove(pi); err != nil {
			return err
		}
	}
	state.Add(client.PackageState{
		UnpackDir:      dir,
		PackageSpec:    zs,
		InstalledFiles: insFiles,
	})
	return nil
}

// Reinstall reinstalls and optionally redownloads, a package.
func Reinstall(ps client.PackageState, state client.GooGetState, rd bool) error {
	pi := goolib.PackageInfo{ps.PackageSpec.Name, ps.PackageSpec.Arch, ps.PackageSpec.Version}
	logger.Infof("Starting reinstall of %s.%s, version %s", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Reinstalling %s.%s %s and dependencies...\n", pi.Name, pi.Arch, pi.Ver)
	_, err := oswrap.Stat(ps.UnpackDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		logger.Infof("Package directory does not exist for %s.%s.%s, redownloading...", pi.Name, pi.Arch, pi.Ver)
		rd = true
	}
	dir := ps.UnpackDir
	if rd {
		if ps.DownloadURL == "" {
			return fmt.Errorf("can not redownload %s.%s.%s, DownloadURL not saved", pi.Name, pi.Arch, pi.Ver)
		}
		dst := ps.UnpackDir + ".goo"
		if err := download.Package(ps.DownloadURL, dst, ps.Checksum); err != nil {
			return fmt.Errorf("error redownloading package: %v", err)
		}
		dir, err = extractPkg(dst)
		if err != nil {
			return err
		}
	}
	if _, err := installPkg(dir, ps.PackageSpec, false); err != nil {
		return fmt.Errorf("error reinstalling package: %v", err)
	}

	logger.Infof("Reinstallation of %s.%s, version %s completed", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Reinstallation of %s.%s %s completed\n", pi.Name, pi.Arch, pi.Ver)
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

func extractPkg(pkg string) (string, error) {
	dir, err := download.ExtractPkg(pkg)
	if err != nil {
		return "", err
	}
	if err := oswrap.Remove(pkg); err != nil {
		logger.Errorf("error cleaning up package file: %v", err)
	}
	return dir, nil
}

// NeedsInstallation checks if a package version needs installation.
func NeedsInstallation(pi goolib.PackageInfo, state client.GooGetState) (bool, error) {
	for _, p := range state {
		if p.PackageSpec.Name == pi.Name {
			if p.PackageSpec.Arch != pi.Arch {
				continue
			}
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
		if err = client.RemoveOrRename(outPath); err != nil {
			return err
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
		// TODO(ajackura): actually use file hash for verification and upgrade.
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

func cleanOldFiles(dir string, oldState client.PackageState, insFiles map[string]string) {
	if len(oldState.InstalledFiles) == 0 {
		return
	}
	var dirs []string
	for file := range oldState.InstalledFiles {
		if chksum, ok := insFiles[file]; !ok {
			if chksum == "" {
				dirs = append(dirs, file)
				continue
			}
			logger.Infof("Cleaning up old file %q", file)
			if err := client.RemoveOrRename(file); err != nil {
				logger.Error(err)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		if err := client.RemoveOrRename(dir); err != nil {
			logger.Info(err)
		}
	}
}

func installPkg(dir string, ps *goolib.PkgSpec, dbOnly bool) (map[string]string, error) {
	logger.Infof("Executing install of package %q", filepath.Base(dir))
	insFiles := make(map[string]string)
	for src, dst := range ps.Files {
		dst = resolveDst(dst)
		src = filepath.Join(dir, src)
		if err := oswrap.Walk(src, makeInstallFunction(src, dst, insFiles, dbOnly)); err != nil {
			return nil, err
		}
	}
	if dbOnly {
		return insFiles, nil
	}
	return insFiles, system.Install(dir, ps)
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
