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
	"path/filepath"

	"github.com/google/googet/goolib"
	"github.com/google/googet/remove"
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

func (cmd *removeCmd) Execute(_ context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	exitCode := subcommands.ExitSuccess

	sf := filepath.Join(rootDir, stateFile)
	state, err := readState(sf)
	if err != nil {
		logger.Error(err)
	}

	for _, arg := range flags.Args() {
		pi := goolib.PkgNameSplit(arg)
		var ins []string
		for _, ps := range *state {
			if ps.Match(pi) {
				ins = append(ins, ps.PackageSpec.Name+"."+ps.PackageSpec.Arch)
			}
		}
		if len(ins) == 0 {
			logger.Errorf("Package %s.%s not installed, cannot remove.", pi.Name, pi.Arch)
			continue
		}
		if len(ins) > 1 {
			fmt.Fprintf(os.Stderr, "More than one %s installed, chose one of:\n%s\n", arg, ins)
			return subcommands.ExitFailure
		}
		pi = goolib.PkgNameSplit(ins[0])
		deps, dl := remove.EnumerateDeps(pi, *state)
		if !noConfirm {
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
		if err = remove.All(pi, deps, state, cmd.dbOnly, proxyServer); err != nil {
			logger.Errorf("error removing %s, %v", arg, err)
			exitCode = subcommands.ExitFailure
			continue
		}
		logger.Infof("Removal of %q and dependant packages completed", pi.Name)
		fmt.Printf("Removal of %s completed\n", pi.Name)
		if err := writeState(state, sf); err != nil {
			logger.Fatalf("error writing state file: %v", err)
		}
	}
	return exitCode
}
