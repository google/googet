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

package main

// The check subcommand searches the repo for packages using the filter provided. The default
// filter is an empty string and will return all packages.

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type checkCmd struct {
	sources string
	dryRun  bool
}

func (*checkCmd) Name() string     { return "check" }
func (*checkCmd) Synopsis() string { return "check and take over existing packages" }
func (*checkCmd) Usage() string {
	return fmt.Sprintf(`%s check [-sources repo1,repo2...] [-dry_run=true]`, filepath.Base(os.Args[0]))
}

func (cmd *checkCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.dryRun, "dry_run", false, "Don't make any changes to the DB.")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
}

func (cmd *checkCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	exitCode := subcommands.ExitFailure
	cache := settings.CacheDir()
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		logger.Fatal(err)
	}
	defer db.Close()
	state, err := db.FetchPkgs("")
	if err != nil {
		logger.Fatal(err)
	}
	downloader, err := client.NewDownloader(settings.ProxyServer)
	if err != nil {
		logger.Fatal(err)
	}
	repos, err := repo.BuildSources(cmd.sources)
	if err != nil {
		logger.Fatal(err)
	}

	rm := downloader.AvailableVersions(ctx, repos, cache, settings.CacheLife)
	unmanaged := make(map[string]string)
	installed := make(map[string]struct{})
	for _, ps := range state {
		installed[ps.PackageSpec.Name] = struct{}{}
	}
	fmt.Println("Searching for unmanaged software...")
	for u, r := range rm {
		for _, p := range r.Packages {
			if _, ok := installed[p.PackageSpec.Name]; ok {
				continue
			}
			app, _ := system.AppAssociation(p.PackageSpec, "")
			if app != "" {
				unmanaged[p.PackageSpec.Name] = app
				if cmd.dryRun {
					logger.Infof("Unmanaged software found during dry_run(packagename: application name): %v: %v\n", p.PackageSpec.Name, app)
					continue
				}
				pi := goolib.PackageInfo{
					Name: p.PackageSpec.Name,
					Arch: p.PackageSpec.Arch,
					Ver:  p.PackageSpec.Version,
				}
				if err := install.FromRepo(ctx, pi, u, cache, rm, settings.Archs, true, downloader, db); err != nil {
					logger.Errorf("Error installing %s.%s.%s: %v", pi.Name, pi.Arch, pi.Ver, err)
					exitCode = subcommands.ExitFailure
					continue
				}
				logger.Infof("Unmanaged software added to googet database(packagename: application name): %v: %v\n", p.PackageSpec.Name, app)
			}
		}
	}
	if len(unmanaged) == 0 {
		fmt.Println("No unmanaged software found.")
		return exitCode
	}
	fmt.Println("Found the following unmanaged software (Package: Software name) ...")
	for _, k := range slices.Sorted(maps.Keys(unmanaged)) {
		fmt.Printf(" %v: %v\n", k, unmanaged[k])
	}
	return exitCode
}
