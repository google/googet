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

// The installed subcommand lists out all installed packages that match the filter.
// The default filter is an empty string and will return all packages.

import (
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/googet/client"
	"github.com/google/googet/goolib"
	"github.com/google/logger"
	"github.com/google/subcommands"
	"golang.org/x/net/context"
)

type installedCmd struct {
	filter string
	info   bool
}

func (*installedCmd) Name() string     { return "installed" }
func (*installedCmd) Synopsis() string { return "list all installed packages" }
func (*installedCmd) Usage() string {
	return fmt.Sprintf("%s installed [-filter <name>] [-info]\n", path.Base(os.Args[0]))
}

func (cmd *installedCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&cmd.filter, "filter", "", "package list filter")
	f.BoolVar(&cmd.info, "info", false, "display package info")
}

func (cmd *installedCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	state, err := readState(filepath.Join(rootDir, stateFile))
	if err != nil {
		logger.Fatal(err)
	}

	pm := installedPackages(*state)
	if len(pm) == 0 {
		fmt.Println("No packages installed.")
		return subcommands.ExitSuccess
	}

	var pl []string
	for p, v := range pm {
		pl = append(pl, p+"."+v)
	}

	sort.Strings(pl)
	if cmd.filter != "" {
		fmt.Printf("Installed packages matching %q:\n", cmd.filter)
	} else {
		fmt.Println("Installed packages:")
	}
	exitCode := subcommands.ExitFailure
	for _, p := range pl {
		if strings.Contains(p, cmd.filter) {
			exitCode = subcommands.ExitSuccess
			pi := goolib.PkgNameSplit(p)
			if cmd.info {
				local(pi, *state)
				continue
			}
			fmt.Println(" ", pi.Name+"."+pi.Arch+" "+pi.Ver)
		}
	}
	if exitCode != subcommands.ExitSuccess {
		fmt.Fprintf(os.Stderr, "No package matching filter %q installed.\n", cmd.filter)
	}
	return exitCode
}

func local(pi goolib.PackageInfo, state client.GooGetState) {
	for _, p := range state {
		if p.Match(pi) {
			info(p.PackageSpec, "installed")
			return
		}
	}
}
