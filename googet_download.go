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

// The download subcommand handles the downloading of a package.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/download"
	"github.com/google/googet/v2/goolib"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type downloadCmd struct {
	downloadDir string
	sources     string
}

func (*downloadCmd) Name() string     { return "download" }
func (*downloadCmd) Synopsis() string { return "download a package" }
func (*downloadCmd) Usage() string {
	return fmt.Sprintf("%s download [-sources repo1,repo2...] [-download_dir <dir>] <name>\n", filepath.Base(os.Args[0]))
}

func (cmd *downloadCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&cmd.downloadDir, "download_dir", "", "directory to download package")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
}

func (cmd *downloadCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if len(flags.Args()) == 0 {
		fmt.Fprintf(os.Stderr, "%s\nUsage: %s\n", cmd.Synopsis(), cmd.Usage())
		return subcommands.ExitFailure
	}
	repos, err := buildSources(cmd.sources)
	if err != nil {
		logger.Fatal(err)
	}
	if repos == nil {
		logger.Fatal("No repos defined, create a .repo file or pass using the -sources flag.")
	}

	rm := client.AvailableVersions(ctx, repos, filepath.Join(rootDir, cacheDir), cacheLife, proxyServer)
	exitCode := subcommands.ExitSuccess

	dir := cmd.downloadDir
	if dir == "" {
		dir, err = os.Getwd()
		if err != nil {
			logger.Fatal(err)
		}
	}

	for _, arg := range flags.Args() {
		pi := goolib.PkgNameSplit(arg)
		if pi.Ver == "" {
			if _, err := download.Latest(ctx, pi.Name, dir, rm, archs, proxyServer); err != nil {
				logger.Errorf("error downloading %s, %v", pi.Name, err)
				exitCode = subcommands.ExitFailure
			}
			continue
		}
		if _, err := goolib.ParseVersion(pi.Ver); err != nil {
			logger.Errorf("invalid package version: %q", pi.Ver)
			exitCode = subcommands.ExitFailure
			continue
		}

		repo, err := client.WhatRepo(pi, rm)
		if err != nil {
			logger.Error(err)
			exitCode = subcommands.ExitFailure
			continue
		}

		rs, err := client.FindRepoSpec(pi, rm[repo])
		if err != nil {
			logger.Error(err)
			exitCode = subcommands.ExitFailure
			continue
		}
		if _, err := download.FromRepo(ctx, rs, repo, dir, proxyServer); err != nil {
			logger.Errorf("error downloading %s.%s %s, %v", pi.Name, pi.Arch, pi.Ver, err)
			exitCode = subcommands.ExitFailure
			continue
		}
	}
	return exitCode
}
