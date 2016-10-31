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

// The update subcommand handles bulk updating of packages.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/googet/client"
	"github.com/google/googet/goolib"
	"github.com/google/googet/install"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type updateCmd struct {
	dbOnly  bool
	sources string
}

func (*updateCmd) Name() string     { return "update" }
func (*updateCmd) Synopsis() string { return "update all packages to the latest version available" }
func (*updateCmd) Usage() string {
	return fmt.Sprintf("%s update [-sources repo1,repo2...]\n", filepath.Base(os.Args[0]))
}

func (cmd *updateCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.dbOnly, "db_only", false, "only make changes to DB, don't perform install system actions")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
}

func (cmd *updateCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	cache := filepath.Join(rootDir, cacheDir)
	sf := filepath.Join(rootDir, stateFile)
	state, err := readState(sf)
	if err != nil {
		logger.Fatal(err)
	}

	pm := installedPackages(*state)
	if len(pm) == 0 {
		fmt.Println("No packages installed.")
		return subcommands.ExitSuccess
	}

	repos, err := buildSources(cmd.sources)
	if err != nil {
		logger.Fatal(err)
	}
	if repos == nil {
		logger.Fatal("No repos defined, create a .repo file or pass using the -sources flag.")
	}

	rm := client.AvailableVersions(repos, filepath.Join(rootDir, cacheDir), cacheLife, proxyServer)
	ud := updates(pm, rm)
	if ud == nil {
		fmt.Println("No updates available for any installed packages.")
		return subcommands.ExitSuccess
	}

	if !noConfirm {
		if !confirmation("Perform update?") {
			fmt.Println("Not updating.")
			return subcommands.ExitSuccess
		}
	}

	exitCode := subcommands.ExitFailure
	for _, pi := range ud {
		r, err := client.WhatRepo(pi, rm)
		if err != nil {
			logger.Errorf("Error finding repo: %v.", err)
		}
		if err := install.FromRepo(pi, r, cache, rm, archs, state, cmd.dbOnly, proxyServer); err != nil {
			logger.Errorf("Error updating %s %s %s: %v", pi.Arch, pi.Name, pi.Ver, err)
			exitCode = subcommands.ExitFailure
			continue
		}
	}

	if err := writeState(state, sf); err != nil {
		logger.Fatalf("Error writing state file: %v", err)
	}

	return exitCode
}

func updates(pm packageMap, rm client.RepoMap) []goolib.PackageInfo {
	fmt.Println("Searching for available updates...")
	var ud []goolib.PackageInfo
	for p, ver := range pm {
		pi := goolib.PkgNameSplit(p)
		v, r, _, err := client.FindRepoLatest(pi, rm, archs)
		if err != nil {
			// This error is because this installed package is not available in a repo.
			logger.Info(err)
			continue
		}
		c, err := goolib.Compare(v, ver)
		if err != nil {
			logger.Error(err)
			continue
		}
		if c == 1 {
			fmt.Printf("  %s, %s --> %s from %s\n", p, ver, v, r)
			logger.Infof("Update for package %s, %s installed and %s available from %s.", p, ver, v, r)
			ud = append(ud, goolib.PackageInfo{pi.Name, pi.Arch, v})
			continue
		}
		logger.Infof("%s - latest version installed", p)
	}
	return ud
}
