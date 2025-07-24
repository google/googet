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
	"errors"
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

func (cmd *installCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if flags.NArg() == 0 {
		fmt.Printf("%s\nUsage: %s\n", cmd.Synopsis(), cmd.Usage())
		return subcommands.ExitFailure
	}
	if cmd.redownload && !cmd.reinstall {
		fmt.Fprintln(os.Stderr, "It's an error to use the -redownload flag without the -reinstall flag")
		return subcommands.ExitFailure
	}

	db, err := googetdb.NewDB(filepath.Join(rootDir, dbFile))
	if err != nil {
		logger.Fatal(err)
	}
	defer db.Close()

	downloader, err := client.NewDownloader(proxyServer)
	if err != nil {
		logger.Fatal(err)
	}

	i := &installer{
		db:              db,
		cache:           filepath.Join(rootDir, cacheDir),
		dbOnly:          cmd.dbOnly,
		shouldReinstall: cmd.reinstall,
		redownload:      cmd.redownload,
		confirm:         !noConfirm,
		downloader:      downloader,
	}

	// We only need to build sources and download indexes if there are any
	// non-file goo arguments passed to the install command (usually the case).
	if !allFileGoos(flag.Args()) {
		repos, err := buildSources(cmd.sources)
		if err != nil {
			logger.Fatal(err)
		}
		if repos == nil {
			logger.Fatal("No repos defined, create a .repo file or pass using the -sources flag.")
		}
		i.repoMap = i.downloader.AvailableVersions(ctx, repos, i.cache, cacheLife)
	}

	var errs error
	for _, arg := range flags.Args() {
		if filepath.Ext(arg) == ".goo" {
			if err := i.installFromFile(arg); err != nil {
				logger.Errorf("Error installing %q from file: %v", arg, err)
				errs = errors.Join(errs, err)
			}
			continue
		}

		// TODO: archs should not be a global variable.
		if err := i.installFromRepo(ctx, arg, archs); err != nil {
			logger.Errorf("Error installing from %q from repo: %v", arg, err)
			errs = errors.Join(errs, err)
		}
	}

	if errs != nil {
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

// allFileGoos returns true if every element of ls represents a path to a .goo
func allFileGoos(ls []string) bool {
	for _, s := range ls {
		if filepath.Ext(s) != ".goo" {
			return false
		}
	}
	return true
}

// installer handles install actions
type installer struct {
	db              *googetdb.GooDB    // the googet database storing package state
	cache           string             // path to cache directory
	downloader      *client.Downloader // HTTP client
	repoMap         client.RepoMap     // packages available for install
	dbOnly          bool               // update database without actually installing
	shouldReinstall bool               // install even if already installed
	redownload      bool               // ignore cached downloads when reinstalling
	confirm         bool               // prompt before changes
}

// installFromFile installs a package from the specified file path.
func (i *installer) installFromFile(path string) error {
	base := filepath.Base(path)
	if i.confirm && !confirmation(fmt.Sprintf("Install %s?", base)) {
		fmt.Printf("Not installing %s...\n", base)
		return nil
	}
	// Pull the whole state to check against local pkgspec.
	state, err := i.db.FetchPkgs("")
	if err != nil {
		return fmt.Errorf("unable to fetch installed packages: %v", err)
	}
	insPkg, err := install.FromDisk(path, i.cache, &state, i.dbOnly, i.shouldReinstall)
	if err != nil {
		return fmt.Errorf("installing %s: %v", path, err)
	}
	if err := i.db.WriteStateToDB(insPkg); err != nil {
		return fmt.Errorf("writing state database: %v", err)
	}
	return nil
}

// installFromRepo installs the named package from a repo.
func (i *installer) installFromRepo(ctx context.Context, name string, archs []string) error {
	pi := goolib.PkgNameSplit(name)
	pkgState, err := i.db.FetchPkg(pi.Name)
	if err != nil {
		return fmt.Errorf("unable to fetch %v: %v", pi.Name, err)
	}
	if i.shouldReinstall {
		if err := i.reinstall(ctx, pi, pkgState); err != nil {
			return fmt.Errorf("reinstalling %s: %v", pi.Name, err)
		}
		if err := i.db.WriteStateToDB(client.GooGetState{pkgState}); err != nil {
			return fmt.Errorf("writing state db: %v", err)
		}
		return nil
	}

	if pi.Ver == "" {
		if pi.Ver, _, pi.Arch, err = client.FindRepoLatest(pi, i.repoMap, archs); err != nil {
			return fmt.Errorf("can't resolve version for package %q: %v", pi.Name, err)
		}
	}
	if _, err := goolib.ParseVersion(pi.Ver); err != nil {
		return fmt.Errorf("invalid package version %q: %v", pi.Ver, err)
	}

	r, err := client.WhatRepo(pi, i.repoMap)
	if err != nil {
		return fmt.Errorf("error finding %s.%s.%s in repo: %v", pi.Name, pi.Arch, pi.Ver, err)
	}
	state := client.GooGetState{pkgState}
	if ni, err := install.NeedsInstallation(pi, state); err != nil {
		return err
	} else if !ni {
		fmt.Printf("%s.%s.%s or a newer version is already installed on the system\n", pi.Name, pi.Arch, pi.Ver)
		return nil
	}
	if i.confirm {
		b, err := enumerateDeps(pi, i.repoMap, r, archs, state)
		if err != nil {
			return err
		}
		if !confirmation(b.String()) {
			fmt.Println("canceling install...")
			return nil
		}
	}
	if err := install.FromRepo(ctx, pi, r, i.cache, i.repoMap, archs, &state, i.dbOnly, i.downloader); err != nil {
		return fmt.Errorf("installing %s.%s.%s: %v", pi.Name, pi.Arch, pi.Ver, err)
	}
	if err := i.db.WriteStateToDB(state); err != nil {
		return fmt.Errorf("writing state file: %v", err)
	}
	return nil
}

func (i *installer) reinstall(ctx context.Context, pi goolib.PackageInfo, ps client.PackageState) error {
	// TODO: Cleanup reinstall logic to remove pi
	if pi.Name == "" {
		return fmt.Errorf("cannot reinstall something that is not already installed")
	}
	if i.confirm {
		if !confirmation(fmt.Sprintf("Reinstall %s?", pi.Name)) {
			fmt.Printf("Not reinstalling %s...\n", pi.Name)
			return nil
		}
	}
	if err := install.Reinstall(ctx, ps, i.redownload, i.downloader); err != nil {
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
