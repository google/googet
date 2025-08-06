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

// The remove subcommand handles the uninstallation of a package.

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type removeCmd struct {
	dbOnly bool
}

func (cmd *removeCmd) Name() string     { return "remove" }
func (cmd *removeCmd) Synopsis() string { return "uninstall a package" }
func (cmd *removeCmd) Usage() string {
	return fmt.Sprintf("%s remove <name>\n", os.Args[0])
}

func (cmd *removeCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.dbOnly, "db_only", false, "only make changes to DB, don't perform uninstall system actions")
}

func (cmd *removeCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	exitCode := subcommands.ExitSuccess

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		logger.Error(err)
	}
	defer db.Close()
	downloader, err := client.NewDownloader(settings.ProxyServer)
	if err != nil {
		logger.Fatal(err)
	}
	for _, arg := range flags.Args() {
		pi := goolib.PkgNameSplit(arg)
		var ins []string
		ps, err := db.FetchPkg(pi.Name)
		if err != nil {
			logger.Fatal(err)
		}
		if ps.PackageSpec == nil {
			logger.Errorf("Package %q not installed, cannot remove.", arg)
			continue
		}
		if ps.Match(pi) {
			ins = append(ins, ps.PackageSpec.Name+"."+ps.PackageSpec.Arch)
		}
		pi = goolib.PkgNameSplit(ins[0])
		deps, dl := remove.EnumerateDeps(pi, db)
		if settings.Confirm {
			var b bytes.Buffer
			fmt.Fprintln(&b, "The following packages will be removed:")
			for _, d := range dl {
				fmt.Fprintln(&b, "  "+d)
			}
			fmt.Fprintf(&b, "Do you wish to remove %s and all dependencies?", pi.Name)
			if !confirmation(b.String()) {
				fmt.Println("canceling removal...")
				continue
			}
		}
		fmt.Printf("Removing %s and all dependencies...\n", pi.Name)

		if err = remove.All(ctx, pi, deps, cmd.dbOnly, downloader, db); err != nil {
			logger.Errorf("error removing %s, %v", arg, err)
			exitCode = subcommands.ExitFailure
			continue
		}
		logger.Infof("Removal of %q and dependant packages completed", pi.Name)
		fmt.Printf("Removal of %s completed\n", pi.Name)
		// TODO: Make sure we aren't removing packages that other packages depend on.
		db.RemovePkg(pi.Name, pi.Arch)
		for _, dep := range dl {
			// We should have strings that look like "packagename.arch version"
			d := strings.SplitN(dep, " ", 2)
			di := goolib.PkgNameSplit(d[0])
			db.RemovePkg(di.Name, di.Arch)
		}

	}
	return exitCode
}
