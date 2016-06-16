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

// The gooserve binary is used to serve GooGet repositories.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
)

var (
	root      = flag.String("root", "", "root location")
	interval  = flag.Duration("interval", 5*time.Minute, "duration between refresh runs")
	verbose   = flag.Bool("verbose", false, "print info level logs to stdout")
	systemLog = flag.Bool("system_log", false, "log to Linux Syslog or Windows Event Log")
	port      = flag.Int("port", 8000, "listen port")
	repoName  = flag.String("repo_name", "repo", "name of the repo to setup")

	repoContents *repoPackages
)

// repoPackages describes a repository of packages.
type repoPackages struct {
	rs []goolib.RepoSpec
	mu sync.Mutex
}

// add provides a thread safe way to add a package to repoPackages.
func (r *repoPackages) add(src, chksum string, spec *goolib.PkgSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rs = append(r.rs, goolib.RepoSpec{
		Source:      src,
		Checksum:    chksum,
		PackageSpec: spec,
	})
}

func packageInfo(pkgPath, packageDir string) error {
	pkg := filepath.Base(pkgPath)
	pi := goolib.PkgNameSplit(strings.TrimSuffix(pkg, ".goo"))

	spec, err := extractSpec(pkgPath)
	if err != nil {
		return err
	}
	if spec.Name != pi.Name {
		return fmt.Errorf("%s: name in spec does not match package file name", pkgPath)
	}
	if spec.Arch != pi.Arch {
		return fmt.Errorf("%s: arch in spec does not match package file name", pkgPath)
	}
	if spec.Version != pi.Ver {
		return fmt.Errorf("%s: version in spec does not match package version", pkgPath)
	}

	f, err := oswrap.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()

	repoContents.add(path.Join(packageDir, pkg), goolib.Checksum(f), spec)
	return nil
}

func runSync(packageDir string) error {
	logger.Info("Beginning sync run")
	if err := oswrap.MkdirAll(packageDir, 0774); err != nil {
		return err
	}

	pkgs, err := filepath.Glob(filepath.Join(packageDir, "*.goo"))
	if err != nil {
		return err
	}

	repoContents = &repoPackages{}
	var wg sync.WaitGroup
	for _, pkg := range pkgs {
		wg.Add(1)
		go func(pkg string) {
			defer wg.Done()
			if err := packageInfo(pkg, packageDir); err != nil {
				logger.Error(err)
			}
		}(pkg)
	}
	wg.Wait()
	logger.Info("Sync run completed successfully")
	return nil
}

// extractSpec takes a goopkg file and returns the unmarshalled spec file.
func extractSpec(pkgPath string) (*goolib.PkgSpec, error) {
	f, err := oswrap.Open(pkgPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return goolib.ExtractPkgSpec(f)
}

func serve(w http.ResponseWriter, r *http.Request) {
	out, err := json.MarshalIndent(repoContents.rs, "", "  ")
	if err != nil {
		logger.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func main() {
	flag.Parse()

	logger.Init("GooServe", *verbose, *systemLog, ioutil.Discard)

	packageDir := filepath.Join(*root, "packages")
	if err := runSync(packageDir); err != nil {
		logger.Error(err)
	}

	http.HandleFunc(fmt.Sprintf("/%s/index", *repoName), serve)
	http.Handle("/packages/", http.StripPrefix("/packages/", http.FileServer(http.Dir(packageDir))))
	go func() {
		err := http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)
		if err != nil {
			logger.Fatal(err)
		}
	}()

	for range time.Tick(*interval) {
		if err := runSync(packageDir); err != nil {
			logger.Error(err)
		}
	}
}
