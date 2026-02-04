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

package remove

// The remove subcommand handles the uninstallation of a package.

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&removeCmd{}, "package management") }

type removeCmd struct {
	dbOnly bool
	dryRun bool
}

func (cmd *removeCmd) Name() string     { return "remove" }
func (cmd *removeCmd) Synopsis() string { return "uninstall a package" }
func (cmd *removeCmd) Usage() string {
	return fmt.Sprintf("%s remove [-dry_run] <name>...\n", os.Args[0])
}

func (cmd *removeCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.dbOnly, "db_only", false, "only make changes to DB, don't perform uninstall system actions")
	f.BoolVar(&cmd.dryRun, "dry_run", false, "show what would be removed but do not remove")
}

func (cmd *removeCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if flags.NArg() == 0 {
		fmt.Printf("%s\nUsage: %s\n", cmd.Synopsis(), cmd.Usage())
		return subcommands.ExitFailure
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		logger.Error(err)
		return subcommands.ExitFailure
	}
	defer db.Close()
	downloader, err := client.NewDownloader(settings.ProxyServer)
	if err != nil {
		logger.Error(err)
		return subcommands.ExitFailure
	}
	status := subcommands.ExitSuccess
	for _, arg := range flags.Args() {
		if err := cmd.removeOne(ctx, arg, downloader, db); err != nil {
			logger.Errorf("error removing %v: %v", arg, err)
			status = subcommands.ExitFailure
			continue
		}
		if !cmd.dryRun {
			logger.Infof("Removal of %q and dependent packages completed", arg)
			fmt.Printf("Removal of %s completed\n", arg)
		}
	}
	return status
}

func (cmd *removeCmd) removeOne(ctx context.Context, pkgName string, downloader *client.Downloader, db *googetdb.GooDB) error {
	pi := goolib.PkgNameSplit(pkgName)
	ps, err := db.FetchPkg(pi.Name)
	if err != nil {
		return err
	}
	if ps.PackageSpec == nil {
		logger.Errorf("Package %q not installed, cannot remove.", pkgName)
		return nil
	}
	pi = goolib.PackageInfo{Name: ps.PackageSpec.Name, Arch: ps.PackageSpec.Arch}
	deps, dl, err := remove.EnumerateDeps(pi, db)
	if err != nil {
		return err
	}

	if cmd.dryRun {
		fmt.Println("Dry run: The following packages would be removed:")
		for _, d := range dl {
			fmt.Println("  " + d)
		}
		fmt.Printf("Dry run: Would remove %s and all dependencies.\n", pi.Name)
		return nil
	}

	if settings.Confirm {
		var b bytes.Buffer
		fmt.Fprintln(&b, "The following packages will be removed:")
		for _, d := range dl {
			fmt.Fprintln(&b, "  "+d)
		}
		fmt.Fprintf(&b, "Do you wish to remove %s and all dependencies?", pi.Name)
		if !cli.Confirmation(b.String()) {
			fmt.Println("canceling removal...")
			return nil
		}
	}
	fmt.Printf("Removing %s and all dependencies...\n", pi.Name)
	return remove.All(ctx, pi, deps, cmd.dbOnly, downloader, db)
}
