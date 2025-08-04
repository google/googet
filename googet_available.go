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

// The available subcommand searches the repo for packages using the filter provided. The default
// filter is an empty string and will return all packages.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type availableCmd struct {
	info    bool
	sources string
}

func (*availableCmd) Name() string     { return "available" }
func (*availableCmd) Synopsis() string { return "list available packages" }
func (*availableCmd) Usage() string {
	return fmt.Sprintf(`%s available [-sources repo1,repo2...] [-info] [<initial>]:
	List available packages beginning with an initial string,
	if no initial string is provided all available packages will be listed.
`, filepath.Base(os.Args[0]))
}

func (cmd *availableCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.info, "info", false, "display package info")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
}

func (cmd *availableCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	exitCode := subcommands.ExitFailure

	var filter string
	switch f.NArg() {
	case 0:
		filter = ""
	case 1:
		filter = f.Arg(0)
	default:
		fmt.Fprintln(os.Stderr, "Excessive arguments")
		f.Usage()
		return subcommands.ExitUsageError
	}

	repos, err := repo.BuildSources(cmd.sources)
	if err != nil {
		logger.Fatal(err)
	}
	if repos == nil {
		logger.Fatal("No repos defined, create a .repo file or pass using the -sources flag.")
	}

	downloader, err := client.NewDownloader(settings.ProxyServer)
	if err != nil {
		logger.Fatal(err)
	}

	m := make(map[string][]string)
	rm := downloader.AvailableVersions(ctx, repos, settings.CacheDir(), settings.CacheLife)
	for r, repo := range rm {
		for _, p := range repo.Packages {
			m[r] = append(m[r], p.PackageSpec.Name+"."+p.PackageSpec.Arch+"."+p.PackageSpec.Version)
		}
	}

	for r, pl := range m {
		logger.Infof("Searching %q for packages matching filter %q.", r, filter)
		sort.Strings(pl)
		i := sort.SearchStrings(pl, filter)
		if i >= len(pl) || !strings.Contains(pl[i], filter) {
			continue
		}
		if !cmd.info {
			fmt.Println(r)
		}
		for _, p := range pl {
			if strings.Contains(p, filter) {
				exitCode = subcommands.ExitSuccess
				pi := goolib.PkgNameSplit(p)
				if cmd.info {
					findAndPrintPackageInfo(pi, rm)
					continue
				}
				fmt.Println(" ", pi.Name+"."+pi.Arch+" "+pi.Ver)
			}
		}
	}

	if exitCode != subcommands.ExitSuccess {
		fmt.Fprintf(os.Stderr, "No package matching filter %q available in any repo.\n", filter)
	}
	return exitCode
}

// findAndPrintPackageInfo finds the repo and PkgSpec matching the given package
// info and then prints out their details.
func findAndPrintPackageInfo(pi goolib.PackageInfo, rm client.RepoMap) {
	for r, repo := range rm {
		for _, p := range repo.Packages {
			if p.PackageSpec.Name == pi.Name && p.PackageSpec.Arch == pi.Arch && p.PackageSpec.Version == pi.Ver {
				info(p.PackageSpec, r)
				return
			}
		}
	}
}
