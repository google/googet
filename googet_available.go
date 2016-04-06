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
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/googet/client"
	"github.com/google/googet/goolib"
	"github.com/google/logger"
	"github.com/google/subcommands"
	"golang.org/x/net/context"
)

type availableCmd struct {
	filter  string
	info    bool
	sources string
}

func (*availableCmd) Name() string     { return "available" }
func (*availableCmd) Synopsis() string { return "list all available packages in repos" }
func (*availableCmd) Usage() string {
	return fmt.Sprintf("%s available [-sources repo1,repo2...] [-filter <name>] [-info]\n", path.Base(os.Args[0]))
}

func (cmd *availableCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&cmd.filter, "filter", "", "package search filter")
	f.BoolVar(&cmd.info, "info", false, "display package info")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
}

func (cmd *availableCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	exitCode := subcommands.ExitFailure

	repos, err := buildSources(cmd.sources)
	if err != nil {
		logger.Fatal(err)
	}
	if repos == nil {
		logger.Fatal("No repos defined, create a .repo file or pass using the -sources flag.")
	}

	m := make(map[string][]string)
	rm := client.AvailableVersions(repos, filepath.Join(rootDir, cacheDir), cacheLife)
	for r, pl := range rm {
		for _, p := range pl {
			m[r] = append(m[r], p.PackageSpec.Name+"."+p.PackageSpec.Arch+"."+p.PackageSpec.Version)
		}
	}

	for r, pl := range m {
		logger.Infof("Searching %q for packages matching filter %q.", r, cmd.filter)
		sort.Strings(pl)
		i := sort.SearchStrings(pl, cmd.filter)
		if i >= len(pl) || !strings.Contains(pl[i], cmd.filter) {
			continue
		}
		if !cmd.info {
			fmt.Println(r)
		}
		for _, p := range pl {
			if strings.Contains(p, cmd.filter) {
				exitCode = subcommands.ExitSuccess
				pi := goolib.PkgNameSplit(p)
				if cmd.info {
					repo(pi, rm)
					continue
				}
				fmt.Println(" ", pi.Name+"."+pi.Arch+" "+pi.Ver)
			}
		}
	}

	if exitCode != subcommands.ExitSuccess {
		fmt.Fprintf(os.Stderr, "No package matching filter %q available in any repo.\n", cmd.filter)
	}
	return exitCode
}

func repo(pi goolib.PackageInfo, rm client.RepoMap) {
	for r, pl := range rm {
		for _, p := range pl {
			if p.PackageSpec.Name == pi.Name && p.PackageSpec.Arch == pi.Arch && p.PackageSpec.Version == pi.Ver {
				info(p.PackageSpec, r)
				return
			}
		}
	}
}
