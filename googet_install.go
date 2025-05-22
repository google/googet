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

// The install subcommand handles the downloading and installation of a package.

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type installCmd struct {
	reinstall  bool
	redownload bool
	dbOnly     bool
	sources    string
}

func (*installCmd) Name() string     { return "install" }
func (*installCmd) Synopsis() string { return "download and install a package and its dependencies" }
func (*installCmd) Usage() string {
	return fmt.Sprintf("%s install [-reinstall] [-sources repo1,repo2...] <name>\n", filepath.Base(os.Args[0]))
}

func (cmd *installCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.reinstall, "reinstall", false, "install even if already installed")
	f.BoolVar(&cmd.redownload, "redownload", false, "redownload package files")
	f.BoolVar(&cmd.dbOnly, "db_only", false, "only make changes to DB, don't perform install system actions")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
}

func (cmd *installCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	db, err := googetdb.NewDB(filepath.Join(rootDir, dbFile))
	if err != nil {
		logger.Fatal(err)
	}
	defer db.Close()
	var state client.GooGetState
	if len(flags.Args()) == 0 {
		fmt.Printf("%s\nUsage: %s\n", cmd.Synopsis(), cmd.Usage())
		return subcommands.ExitFailure
	}

	if cmd.redownload && !cmd.reinstall {
		fmt.Fprintln(os.Stderr, "It's an error to use the -redownload flag without the -reinstall flag")
		return subcommands.ExitFailure
	}

	args := flags.Args()
	exitCode := subcommands.ExitSuccess

	cache := filepath.Join(rootDir, cacheDir)

	if len(args) == 0 {
		return exitCode
	}

	repos, err := buildSources(cmd.sources)
	if err != nil {
		logger.Fatal(err)
	}

	downloader, err := client.NewDownloader(proxyServer)
	if err != nil {
		logger.Fatal(err)
	}

	var rm client.RepoMap
	for _, arg := range args {
		if ext := filepath.Ext(arg); ext == ".goo" {
			if !noConfirm {
				if base := filepath.Base(arg); !confirmation(fmt.Sprintf("Install %s?", base)) {
					fmt.Printf("Not installing %s...\n", base)
					continue
				}
			}
			// Pull the whole state to check against local pkgspec.
			state, err = db.FetchPkgs()
			if err != nil {
				logger.Fatalf("Unable to fetch installed packges: %v", err)
			}
			insPkg, err := install.FromDisk(arg, cache, &state, cmd.dbOnly, cmd.reinstall)
			if err != nil {
				logger.Errorf("Error installing %s: %v", arg, err)
				exitCode = subcommands.ExitFailure
				continue
			}
			if err := db.WriteStateToDB(insPkg); err != nil {
				logger.Fatalf("Error writing state database: %v", err)
			}
			continue
		}

		pi := goolib.PkgNameSplit(arg)
		pkgState, err := db.FetchPkg(pi.Name)
		if err != nil {
			logger.Fatalf("Unable to fetch %v: %v", pi.Name, err)
		}
		if cmd.reinstall {
			if err := reinstall(ctx, pi, pkgState, cmd.redownload, downloader); err != nil {
				logger.Errorf("Error reinstalling %s: %v", pi.Name, err)
				exitCode = subcommands.ExitFailure
				continue
			}
			if err := db.WriteStateToDB(client.GooGetState{pkgState}); err != nil {
				logger.Fatalf("Error writing state db: %v", err)
			}
			continue
		}
		if len(rm) == 0 {
			if repos == nil {
				logger.Fatal("No repos defined, create a .repo file or pass using the -sources flag.")
			}
			rm = downloader.AvailableVersions(ctx, repos, filepath.Join(rootDir, cacheDir), cacheLife)
		}
		if pi.Ver == "" {
			v, _, a, err := client.FindRepoLatest(pi, rm, archs)
			pi.Ver, pi.Arch = v, a
			if err != nil {
				logger.Errorf("Can't resolve version for package %q: %v", pi.Name, err)
				exitCode = subcommands.ExitFailure
				continue
			}
		}
		if _, err := goolib.ParseVersion(pi.Ver); err != nil {
			logger.Errorf("Invalid package version %q: %v", pi.Ver, err)
			exitCode = subcommands.ExitFailure
			continue
		}

		r, err := client.WhatRepo(pi, rm)
		if err != nil {
			logger.Errorf("Error finding %s.%s.%s in repo: %v", pi.Name, pi.Arch, pi.Ver, err)
			exitCode = subcommands.ExitFailure
			continue
		}
		state = client.GooGetState{pkgState}
		ni, err := install.NeedsInstallation(pi, state)
		if err != nil {
			logger.Error(err)
			exitCode = subcommands.ExitFailure
			continue
		}
		if !ni {
			fmt.Printf("%s.%s.%s or a newer version is already installed on the system\n", pi.Name, pi.Arch, pi.Ver)
			continue
		}
		if !noConfirm {
			b, err := enumerateDeps(pi, rm, r, archs, state)
			if err != nil {
				logger.Error(err)
				exitCode = subcommands.ExitFailure
				continue
			}
			if !confirmation(b.String()) {
				fmt.Println("canceling install...")
				continue
			}
		}
		if err := install.FromRepo(ctx, pi, r, cache, rm, archs, &state, cmd.dbOnly, downloader); err != nil {
			logger.Errorf("Error installing %s.%s.%s: %v", pi.Name, pi.Arch, pi.Ver, err)
			exitCode = subcommands.ExitFailure
			continue
		}
		if err := db.WriteStateToDB(state); err != nil {
			logger.Fatalf("error writing state file: %v", err)
		}
	}
	return exitCode
}

func reinstall(ctx context.Context, pi goolib.PackageInfo, ps client.PackageState, rd bool, downloader *client.Downloader) error {
	// TODO: Cleanup reinstall logic to remove pi
	if pi.Name == "" {
		return fmt.Errorf("cannot reinstall something that is not already installed")
	}
	if !noConfirm {
		if !confirmation(fmt.Sprintf("Reinstall %s?", pi.Name)) {
			fmt.Printf("Not reinstalling %s...\n", pi.Name)
			return nil
		}
	}
	if err := install.Reinstall(ctx, ps, rd, downloader); err != nil {
		return fmt.Errorf("error reinstalling %s, %v", pi.Name, err)
	}
	return nil
}

func enumerateDeps(pi goolib.PackageInfo, rm client.RepoMap, r string, archs []string, state client.GooGetState) (*bytes.Buffer, error) {
	dl, err := install.ListDeps(pi, rm, r, archs)
	if err != nil {
		return nil, fmt.Errorf("error listing dependencies for %s.%s.%s: %v", pi.Name, pi.Arch, pi.Ver, err)
	}
	var b bytes.Buffer
	fmt.Fprintln(&b, "The following packages will be installed:")
	for _, di := range dl {
		ni, err := install.NeedsInstallation(di, state)
		if err != nil {
			return nil, err
		}
		if ni {
			fmt.Fprintf(&b, "  %s.%s.%s\n", di.Name, di.Arch, di.Ver)
		}
	}
	fmt.Fprintf(&b, "Do you wish to install %s.%s.%s and all dependencies?", pi.Name, pi.Arch, pi.Ver)
	return &b, nil
}
