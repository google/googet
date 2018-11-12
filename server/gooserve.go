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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
	"google.golang.org/api/iterator"
)

var (
	root        = flag.String("root", "", "root location, leave empty for GCS path")
	interval    = flag.Duration("interval", 5*time.Minute, "duration between refresh runs")
	verbose     = flag.Bool("verbose", false, "print info level logs to stdout")
	systemLog   = flag.Bool("system_log", false, "log to Linux Syslog or Windows Event Log")
	address     = flag.String("address", "", "address to listen on")
	port        = flag.Int("port", 8000, "listen port")
	repoName    = flag.String("repo_name", "repo", "name of the repo to setup")
	packagePath = flag.String("package_path", "packages", "path under both the filesystem (-root flag) and webserver root where packages are located, for GCS paths set the full path here and leave -root empty")
	dumpIndex   = flag.Bool("dump_index", false, "dump the package index to stdout and quit")
	saveIndex   = flag.String("save_index", "", "save the package index to the specified file and quit")

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

func runSync(ctx context.Context, rootLoc, packageLoc string) error {
	logger.Info("Beginning sync run")

	var pkgs []string
	var err error
	var client *storage.Client

	isGCSURL, bucket, folder := goolib.SplitGCSUrl(packageLoc)
	if isGCSURL {
		logger.Infof("Scanning GCS bucket %q, prefix %q for packages...", bucket, folder)
		client, err = storage.NewClient(ctx)
		if err != nil {
			return err
		}
		defer client.Close()

		it := client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: folder})
		for objAttr, err := it.Next(); err != iterator.Done; objAttr, err = it.Next() {
			if err != nil {
				return err
			}
			if objAttr.Size == 0 {
				continue
			}

			if strings.HasSuffix(objAttr.Name, ".goo") {
				pkgs = append(pkgs, objAttr.Name)
			}
		}
	} else {
		packageDir := filepath.Join(rootLoc, packageLoc)
		logger.Infof("Scanning directory %q for packages...", packageDir)
		if err := oswrap.MkdirAll(packageDir, 0774); err != nil {
			return err
		}
		pkgs, err = filepath.Glob(filepath.Join(packageDir, "*.goo"))
		if err != nil {
			return err
		}
	}

	repoContents = &repoPackages{}
	var wg sync.WaitGroup
	for _, pkgPath := range pkgs {
		wg.Add(1)
		go func(pkgPath string) {
			defer wg.Done()

			var r io.ReadCloser
			if isGCSURL {
				pkgURI := fmt.Sprintf("%s/%s", *packagePath, pkgPath)
				logger.Infof("Reading package %q", pkgURI)
				r, err = client.Bucket(bucket).Object(pkgPath).NewReader(ctx)
				if err != nil {
					logger.Error(err)
					return
				}
				defer r.Close()
			} else {
				pkgPath = path.Join(*packagePath, filepath.Base(pkgPath))
				logger.Infof("Reading package %q", pkgPath)
				r, err = oswrap.Open(pkgPath)
				if err != nil {
					logger.Error(err)
					return
				}
				defer r.Close()
			}

			spec, err := goolib.ExtractPkgSpec(r)
			if err != nil {
				logger.Error(err)
				return
			}

			repoContents.add(pkgPath, goolib.Checksum(r), spec)
		}(pkgPath)
	}
	wg.Wait()
	logger.Info("Sync run completed successfully")
	return nil
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
	ctx := context.Background()
	logger.Init("GooServe", *verbose, *systemLog, ioutil.Discard)

	if err := runSync(ctx, *root, *packagePath); err != nil {
		logger.Error(err)
	}

	if *dumpIndex || *saveIndex != "" {
		out, err := json.MarshalIndent(repoContents.rs, "", "  ")
		if err != nil {
			logger.Fatal(err)
		}
		if *dumpIndex {
			fmt.Println(string(out))
		}
		if *saveIndex != "" {
			logger.Infof("Writing index to %q", *saveIndex)
			if isGCSURL, bucket, object := goolib.SplitGCSUrl(*saveIndex); isGCSURL {
				client, err := storage.NewClient(ctx)
				if err != nil {
					logger.Fatal(err)
				}
				defer client.Close()

				w := client.Bucket(bucket).Object(object).NewWriter(ctx)
				if _, err := w.Write(out); err != nil {
					logger.Fatal(err)
				}
				if err := w.Close(); err != nil {
					logger.Fatal(err)
				}
			} else {
				err := ioutil.WriteFile(*saveIndex, out, 0644)
				if err != nil {
					logger.Fatal(err)
				}
			}
		}
		return
	}

	http.HandleFunc(fmt.Sprintf("/%s/index", *repoName), serve)
	prefix := "/" + *packagePath + "/"
	http.Handle(prefix, http.StripPrefix(prefix, http.FileServer(http.Dir(filepath.Join(*root, *packagePath)))))
	go func() {
		err := http.ListenAndServe(fmt.Sprintf("%s:%d", *address, *port), nil)
		if err != nil {
			logger.Fatal(err)
		}
	}()

	for range time.Tick(*interval) {
		if err := runSync(ctx, *root, *packagePath); err != nil {
			logger.Error(err)
		}
	}
}
