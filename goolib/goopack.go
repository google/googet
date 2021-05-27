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

package goolib

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

)

type fileMap map[string][]string

type pathWalk struct {
	parts     [][]string
	firstGlob int
}

//CreatePackage accepts a goospec file and generates the packaged goo file
func CreatePackage(gs *GooSpec, baseDir, outDir string) (string, error) {
	switch {
	case gs.Build.Linux != "" && runtime.GOOS == "linux":
		cmd := gs.Build.Linux
		if !filepath.IsAbs(cmd) {
			cmd = filepath.Join(baseDir, cmd)
		}
		if err := Exec(cmd, gs.Build.LinuxArgs, nil, ioutil.Discard); err != nil {
			return "",err
		}
	case gs.Build.Windows != "" && runtime.GOOS == "windows":
		cmd := gs.Build.Windows
		if !filepath.IsAbs(cmd) {
			cmd = filepath.Join(baseDir, cmd)
		}
		if err := Exec(cmd, gs.Build.WindowsArgs, nil, ioutil.Discard); err != nil {
			return "",err
		}
	}
	fm, err := mapFiles(gs.Sources)
	if err != nil {
		return "",err
	}
	if err := verifyFiles(gs, fm); err != nil {
		return "",err
	}
	return packageFiles(fm, gs, outDir)
}

// walkDir returns a list of all files in directory and subdirectories, it is similar
// to filepath.Walk but works even if dir is a symlink.
func walkDir(dir string) ([]string, error) {
	rl, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var wl []string
	for _, fi := range rl {
		path := filepath.Join(dir, fi.Name())

		// follow symlinks
		if (fi.Mode() & os.ModeSymlink) != 0 {
			if fi, err = oswrap.Stat(path); err != nil {
				return nil, err
			}
		}
		if !fi.IsDir() {
			wl = append(wl, path)
			continue
		}
		l, err := walkDir(path)
		if err != nil {
			return nil, err
		}
		wl = append(wl, l...)
	}
	return wl, nil
}

// mergeWalks reduces the number of filesystem walks needed. If one walk will
// cover all the paths in another walk, it merges the include patterns, and only
// the larger walk will be performed.
func mergeWalks(walks []pathWalk) []pathWalk {
	for i := len(walks) - 2; i >= 0; i-- {
		wi := &walks[i]
		for j := i + 1; j < len(walks); j++ {
			wj := &walks[j]
			lowGlob := min(wi.firstGlob, wj.firstGlob)
			if lowGlob < 0 {
				continue
			}
			if filepath.Join(wi.parts[0][:lowGlob]...) == filepath.Join(wj.parts[0][:lowGlob]...) {
				wi.parts = append(wi.parts, wj.parts...)
				wi.firstGlob = lowGlob
				if j+1 < len(walks) {
					walks = append(walks[:j], walks[j+1:]...)
				} else {
					walks = walks[:j]
				}
			}
		}
	}
	return walks
}

func glob(base string, includes, excludes []string) ([]string, error) {
	var pathincludes []string
	for _, in := range includes {
		pathincludes = append(pathincludes, filepath.Join(base, in))
	}
	var pathexcludes []string
	for _, ex := range excludes {
		pathexcludes = append(pathexcludes, filepath.Join(base, ex))
	}

	var walks []pathWalk
	for _, pi := range pathincludes {
		parts := [][]string{splitPath(pi)}
		if !strings.Contains(pi, "*") {
			walks = append(walks, pathWalk{parts, -1})
			continue
		}
		firstGlob := -1
		for i, part := range parts[0] {
			if strings.Contains(part, "*") {
				firstGlob = i
				break
			}
		}
		walks = append(walks, pathWalk{parts, firstGlob})
	}

	walks = mergeWalks(walks)

	var out []string
	for _, walk := range walks {
		if walk.firstGlob < 0 {
			out = append(out, filepath.Join(walk.parts[0]...))
			continue
		}
		wd := strings.Join(walk.parts[0][:walk.firstGlob], string(filepath.Separator))
		files, err := walkDir(wd)
		if err != nil {
			return nil, fmt.Errorf("walking %s: %v", wd, err)
		}
		var walkincludes []string
		for _, p := range walk.parts {
			path := filepath.Clean(strings.Join(p, string(filepath.Separator)))
			walkincludes = append(walkincludes, path)
		}
		for _, file := range files {
			keep, err := anyMatch(walkincludes, file)
			if err != nil {
				return nil, err
			}
			remove, err := anyMatch(pathexcludes, file)
			if err != nil {
				return nil, err
			}
			if keep && !remove {
				out = append(out, file)
			}
		}
	}
	return out, nil
}

func anyMatch(patterns []string, name string) (bool, error) {
	for _, ex := range patterns {
		m, err := pathMatch(ex, name)
		if err != nil {
			return false, err
		}
		if m {
			return true, nil
		}
	}
	return false, nil
}

func min(i, j int) int {
	if i < j {
		return i
	}
	return j
}

func writeFiles(tw *tar.Writer, fm fileMap) error {
	for folder, fl := range fm {
		for _, file := range fl {
			fi, err := oswrap.Stat(file)
			if err != nil {
				return err
			}
			fpath := filepath.Join(folder, filepath.Base(file))
			fih, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return err
			}
			fih.Name = filepath.ToSlash(fpath)
			if err := tw.WriteHeader(fih); err != nil {
				return err
			}
			f, err := oswrap.Open(file)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

// pathMatch is a simpler filepath.Match but which supports recursive globbing
// (**) and doesn't get any more special than * or **.

func pathMatch(pattern, path string) (bool, error) {
	regex := []rune("^")
	runePattern := []rune(pattern)
	for i := 0; i < len(runePattern); i++ {
		ch := runePattern[i]
		switch ch {
		default:
			regex = append(regex, ch)
		case '%', '\\', '(', ')', '[', ']', '.', '^', '$', '?', '+', '{', '}', '=':
			regex = append(regex, '\\', ch)
		case '*':
			if i+1 < len(runePattern) && runePattern[i+1] == '*' {
				if i+2 < len(runePattern) && runePattern[i+2] == '*' {
					return false, fmt.Errorf("%s: malformed glob", pattern)
				}
				regex = append(regex, []rune(".*")...)
				i++
			} else {
				regex = append(regex, []rune("[^/]*")...)
			}
		}
	}
	regex = append(regex, '$')
	re, err := regexp.Compile(string(regex))
	if err != nil {
		return false, err
	}
	return re.MatchString(path), nil
}

func globFiles(s PkgSources) ([]string, error) {
	cr := filepath.Clean(s.Root)
	return glob(cr, s.Include, s.Exclude)
}

func mapFiles(sources []PkgSources) (fileMap, error) {
	fm := make(fileMap)
	for _, s := range sources {
		fl, err := globFiles(s)
		if err != nil {
			return nil, err
		}
		for _, f := range fl {
			dir := strings.TrimPrefix(filepath.Dir(f), s.Root)
			// Ensure leading '/' is trimmed for directories.
			dir = strings.TrimPrefix(dir, string(filepath.Separator))
			tgt := filepath.Join(s.Target, dir)
			fm[tgt] = append(fm[tgt], f)
		}
	}
	return fm, nil
}

func splitPath(path string) []string {
	parts := strings.Split(filepath.Clean(path), string(os.PathSeparator))
	out := []string{}
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	if len(path) > 0 && path[0] == '/' {
		out = append([]string{"/"}, out...)
	}
	return out
}

func verifyFiles(gs *GooSpec, fm fileMap) error {
	fs := make(map[string]bool)
	for folder, fl := range fm {
		parts := splitPath(folder)
		for i := range parts {
			fs[filepath.Join(parts[:i+1]...)] = true
		}
		folder = filepath.Join(parts...)
		for _, file := range fl {
			fpath := filepath.Join(folder, filepath.Base(file))
			fs[fpath] = true
		}
	}
	var missing []string
	for src := range gs.PackageSpec.Files {
		if !fs[src] {
			missing = append(missing, src)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("requested files %v not in package", missing)
	}
	return nil
}

func packageFiles(fm fileMap, gs *GooSpec, dir string) (PkgLoc string, err error) {
	pn := PackageInfo{Name: gs.PackageSpec.Name, Arch: gs.PackageSpec.Arch, Ver: gs.PackageSpec.Version}.PkgName()
	f, err := oswrap.Create(filepath.Join(dir, pn))
	if err != nil {
		return "", err
	}
	defer func() {
		cErr := f.Close()
		if cErr != nil && err == nil {
			err = cErr
		}
	}()
	gw := gzip.NewWriter(f)
	defer func() {
		cErr := gw.Close()
		if cErr != nil && err == nil {
			err = cErr
		}
	}()
	tw := tar.NewWriter(gw)
	defer func() {
		cErr := tw.Close()
		if cErr != nil && err == nil {
			err = cErr
		}
	}()

	if err := writeFiles(tw, fm); err != nil {
		return "", err
	}

	return f.Name(), WritePackageSpec(tw, gs.PackageSpec)
}

