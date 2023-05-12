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

// The latest subcommand searches the repo for the specified package and returns the latest version.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type latestCmd struct {
	compare bool
	sources string
}

func (*latestCmd) Name() string     { return "latest" }
func (*latestCmd) Synopsis() string { return "print the latest available version of a package" }
func (*latestCmd) Usage() string {
	return fmt.Sprintf("%s latest [-sources repo1,repo2...] [-compare] <name>\n", filepath.Base(os.Args[0]))
}

func (cmd *latestCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.compare, "compare", false, "compare to version locally installed")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
}

func (cmd *latestCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	pi := goolib.PkgNameSplit(flags.Arg(0))

	repos, err := buildSources(cmd.sources)
	if err != nil {
		logger.Fatal(err)
	}
	if repos == nil {
		logger.Fatal("No repos defined, create a .repo file or pass using the -sources flag.")
	}

	rm := client.AvailableVersions(ctx, repos, filepath.Join(rootDir, cacheDir), cacheLife, proxyServer, insecure)
	v, _, a, err := client.FindRepoLatest(pi, rm, archs)
	if err != nil {
		logger.Fatal(err)
	}
	if !cmd.compare {
		fmt.Println(v)
		return subcommands.ExitSuccess
	}

	state, err := readState(filepath.Join(rootDir, stateFile))
	if err != nil {
		logger.Fatal(err)
	}
	pi.Arch = a
	var ver string
	for _, p := range *state {
		if p.Match(pi) {
			ver = p.PackageSpec.Version
			break
		}
		fmt.Println(v)
		return subcommands.ExitSuccess
	}
	c, err := goolib.Compare(v, ver)
	if err != nil {
		logger.Fatal(err)
	}
	if c == -1 {
		fmt.Println(ver)
		return subcommands.ExitSuccess
	}
	fmt.Println(v)
	return subcommands.ExitSuccess
}
