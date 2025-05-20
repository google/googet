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

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type cleanCmd struct {
	all      bool
	packages string
}

func (*cleanCmd) Name() string     { return "clean" }
func (*cleanCmd) Synopsis() string { return "clean the cache directory" }
func (*cleanCmd) Usage() string {
	return fmt.Sprintf("%s clean\n", filepath.Base(os.Args[0]))
}

func (cmd *cleanCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.all, "all", false, "clear out the entire cache directory")
	f.StringVar(&cmd.packages, "packages", "", "comma separated list of packages to clear out of the cache")
}

func (cmd *cleanCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if cmd.all {
		fmt.Println("Removing all files and directories in cachedir.")
		clean(nil)
	} else if cmd.packages != "" {
		pl := strings.Split(cmd.packages, ",")
		fmt.Printf("Removing package cache for %s\n", pl)
		cleanPackages(pl)
	} else {
		fmt.Println("Removing all files and directories in cachedir that don't correspond to a currently installed package.")
		cleanOld()
	}
	return subcommands.ExitSuccess
}

func cleanPackages(pl []string) {
	db, err := googetdb.NewDB(filepath.Join(rootDir, dbFile))
	if err != nil {
		logger.Fatal(err)
	}
	state, err := db.FetchPkgs()
	if err != nil {
		logger.Fatal(err)
	}

	for _, pkg := range state {
		if goolib.ContainsString(pkg.PackageSpec.Name, pl) {
			if err := oswrap.RemoveAll(pkg.LocalPath); err != nil {
				logger.Error(err)
			}
		}
	}
}

func clean(il []string) {
	files, err := filepath.Glob(filepath.Join(rootDir, cacheDir, "*"))
	if err != nil {
		logger.Fatal(err)
	}
	for _, file := range files {
		if !goolib.ContainsString(file, il) {
			if err := oswrap.RemoveAll(file); err != nil {
				logger.Error(err)
			}
		}
	}
}

func cleanOld() {
	state, err := readState(filepath.Join(rootDir, stateFile))
	if err != nil {
		logger.Fatal(err)
	}

	var il []string
	for _, pkg := range state {
		il = append(il, pkg.LocalPath)
	}
	clean(il)
}
