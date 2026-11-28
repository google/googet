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

package clean

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&cleanCmd{}, "") }

type cleanCmd struct {
	all      bool
	packages string
}

func (*cleanCmd) Name() string     { return "clean" }
func (*cleanCmd) Synopsis() string { return "clean the cache directory" }
func (*cleanCmd) Usage() string {
	return fmt.Sprintf("%s clean\n", filepath.Base(os.Args[0]))
}

// NoPositionals marks clean as a command that takes no positional arguments.
func (*cleanCmd) NoPositionals() {}

var _ cli.NoPositionals = (*cleanCmd)(nil)

func (cmd *cleanCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.all, "all", false, "clear out the entire cache directory")
	f.StringVar(&cmd.packages, "packages", "", "comma separated list of packages to clear out of the cache")
}

func (cmd *cleanCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if cli.RefuseUnlessPrivileged(ctx) {
		logger.Error("Cleaning the cache requires administrator privileges.")
		return subcommands.ExitFailure
	}
	if cmd.all {
		fmt.Println("Removing all files and directories in cachedir.")
		if err := cleanDirectory(nil); err != nil {
			logger.Error(err)
			return subcommands.ExitFailure
		}
		return subcommands.ExitSuccess
	}

	db, err := cli.OpenDB(ctx)
	if err != nil {
		logger.Errorf("Failed to open database: %v", err)
		return subcommands.ExitFailure
	}
	defer db.Close()
	state, err := db.FetchPkgs("")
	if err != nil {
		logger.Errorf("Failed fetching installed packages: %v", err)
		return subcommands.ExitFailure
	}

	if cmd.packages != "" {
		pl := strings.Split(cmd.packages, ",")
		included := make(map[string]bool)
		for _, name := range pl {
			included[name] = true
		}
		fmt.Printf("Removing package cache for %s\n", pl)
		cleanInstalled(state, included)
		return subcommands.ExitSuccess
	}

	fmt.Println("Removing all files and directories in cachedir that don't correspond to a currently installed package.")
	if err := cleanUninstalled(state); err != nil {
		logger.Error(err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

// cleanInstalled deletes the cached files for installed packages that are
// specified in the included map.
func cleanInstalled(state client.GooGetState, included map[string]bool) {
	for _, pkg := range state {
		if !included[pkg.PackageSpec.Name] {
			continue
		}
		if err := removeCacheEntry(pkg.LocalPath); err != nil {
			logger.Error(err)
		}
	}
}

// removeCacheEntry removes path from the cache directory. A file, such as a
// repo index cache that an unelevated reader may hold open, is removed with
// oswrap.RenameAndRemove so that its path is never left delete pending for the
// next write. An empty path, such as the LocalPath of a package with no cached
// file, names nothing and is skipped. Anything else, such as an unpack
// directory or a missing path, goes to oswrap.RemoveAll.
func removeCacheEntry(path string) error {
	if path == "" {
		return nil
	}
	if fi, err := oswrap.Lstat(path); err == nil && !fi.IsDir() {
		return oswrap.RenameAndRemove(path)
	}
	return oswrap.RemoveAll(path)
}

// cleanDirectory deletes all files in the cache directory except those whose path
// appears in the excluded map.
func cleanDirectory(excluded map[string]bool) error {
	files, err := filepath.Glob(filepath.Join(settings.CacheDir(), "*"))
	if err != nil {
		return err
	}
	for _, file := range files {
		if excluded[file] {
			continue
		}
		if err := removeCacheEntry(file); err != nil {
			logger.Error(err)
		}
	}
	return nil
}

// cleanUninstalled deletes all files in the cache directory except those that
// correspond to an installed package in state.
func cleanUninstalled(state client.GooGetState) error {
	excluded := make(map[string]bool)
	for _, pkg := range state {
		excluded[pkg.LocalPath] = true
	}
	return cleanDirectory(excluded)
}
