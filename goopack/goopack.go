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

// The goopack binary creates a GooGet package using the provided GooSpec file.
package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
)

var outputDir = flag.String("output_dir", "", "where to put the built package")

type fileMap map[string][]string

// walkDir returns a list of all files in directory and subdirectories, it is similar
// to filepath.Walk but works even if dir is a symlink, which is the case with blaze Filesets.
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

type pathWalk struct {
	parts     [][]string
	firstGlob int
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
		wd := filepath.Join(walk.parts[0][:walk.firstGlob]...)
		files, err := walkDir(wd)
		if err != nil {
			return nil, fmt.Errorf("walking %s: %v", wd, err)
		}

		var walkincludes []string
		for _, p := range walk.parts {
			walkincludes = append(walkincludes, filepath.Join(p...))
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

func globFiles(s goolib.PkgSources) ([]string, error) {
	cr := filepath.Clean(s.Root)
	return glob(cr, s.Include, s.Exclude)
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

func packageFiles(fm fileMap, gs goolib.GooSpec, dir string) (err error) {
	pn := goolib.PackageInfo{gs.PackageSpec.Name, gs.PackageSpec.Arch, gs.PackageSpec.Version}.PkgName()
	f, err := oswrap.Create(filepath.Join(dir, pn))
	if err != nil {
		return err
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
		return err
	}

	return goolib.WritePackageSpec(tw, gs.PackageSpec)
}

func mapFiles(sources []goolib.PkgSources) (fileMap, error) {
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

func verifyFiles(gs goolib.GooSpec, fm fileMap) error {
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

func createPackage(gs goolib.GooSpec, baseDir, outDir string) error {
	switch {
	case gs.Build.Linux != "" && runtime.GOOS == "linux":
		cmd := gs.Build.Linux
		if !filepath.IsAbs(cmd) {
			cmd = filepath.Join(baseDir, cmd)
		}
		if err := goolib.Exec(cmd, gs.Build.LinuxArgs, nil, ioutil.Discard); err != nil {
			return err
		}
	case gs.Build.Windows != "" && runtime.GOOS == "windows":
		cmd := gs.Build.Windows
		if !filepath.IsAbs(cmd) {
			cmd = filepath.Join(baseDir, cmd)
		}
		if err := goolib.Exec(cmd, gs.Build.WindowsArgs, nil, ioutil.Discard); err != nil {
			return err
		}
	}
	fm, err := mapFiles(gs.Sources)
	if err != nil {
		return err
	}
	if err := verifyFiles(gs, fm); err != nil {
		return err
	}
	return packageFiles(fm, gs, outDir)
}

func usage() {
	fmt.Printf("Usage: %s <path/to/goospec>\n", filepath.Base(os.Args[0]))
}

func main() {
	flag.Parse()
	switch len(flag.Args()) {
	case 0:
		fmt.Println("Not enough args.")
		usage()
		os.Exit(1)
	case 1:
	default:
		fmt.Println("Too many args.")
		usage()
		os.Exit(1)
	}
	if flag.Arg(1) == "help" {
		usage()
		os.Exit(0)
	}

	outDir := *outputDir
	if outDir == "" {
		var err error
		outDir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}
	gs, err := goolib.ReadGooSpec(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	baseDir := filepath.Dir(filepath.Clean(flag.Arg(0)))
	if baseDir == "." {
		baseDir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}
	if err := createPackage(gs, baseDir, outDir); err != nil {
		log.Fatal(err)
	}
}
