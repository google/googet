/*
Copyright 2018 Google Inc. All Rights Reserved.
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

// The verify subcommand handles verifying of packages.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/verify"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type verifyCmd struct {
	reinstall bool
	skipFiles bool
}

func (*verifyCmd) Name() string     { return "verify" }
func (*verifyCmd) Synopsis() string { return "verify a package, and reinstall if needed" }
func (*verifyCmd) Usage() string {
	return fmt.Sprintf("%s [-noconfirm] verify [-reinstall] [-skip_files] <name>\n", filepath.Base(os.Args[0]))
}

func (cmd *verifyCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.reinstall, "reinstall", false, "reinstall package if verify fails")
	f.BoolVar(&cmd.skipFiles, "skip_files", false, "skip checksum verification of files installed by GooGet")
}

func (cmd *verifyCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if len(flags.Args()) == 0 {
		fmt.Printf("%s\nUsage: %s\n", cmd.Synopsis(), cmd.Usage())
		return subcommands.ExitFailure
	}
	exitCode := subcommands.ExitSuccess

	sf := filepath.Join(rootDir, stateFile)
	state, err := readState(sf)
	if err != nil {
		logger.Error(err)
	}

	for _, arg := range flags.Args() {
		pi := goolib.PkgNameSplit(arg)
		ps, err := state.GetPackageState(pi)
		if err != nil {
			logger.Errorf("Package %q not installed, cannot verify.", arg)
			continue
		}
		pkg := fmt.Sprintf("%s.%s.%s", ps.PackageSpec.Name, ps.PackageSpec.Arch, ps.PackageSpec.Version)

		// Check for multiples.
		var ins []string
		for _, p := range *state {
			if p.Match(pi) {
				ins = append(ins, p.PackageSpec.Name+"."+p.PackageSpec.Arch)
			}
		}
		if len(ins) > 1 {
			fmt.Fprintf(os.Stderr, "More than one %s installed, chose one of:\n%s\n", arg, ins)
			exitCode = subcommands.ExitFailure
			continue
		}

		v, err := verify.Command(ctx, ps, proxyServer)
		if err != nil {
			logger.Errorf("Error running verify command for %s: %v", pkg, err)
			exitCode = subcommands.ExitFailure
			continue
		}

		if v && !cmd.skipFiles {
			v, err = verify.Files(ps)
			if err != nil {
				logger.Errorf("Error running file verification for %s: %v", pkg, err)
				exitCode = subcommands.ExitFailure
				continue
			}
		}
		if !v && cmd.reinstall {
			msg := fmt.Sprintf("Verification failed for %s, reinstalling...", pkg)
			logger.Info(msg)
			fmt.Println(msg)
			if err := install.Reinstall(ctx, ps, *state, false, proxyServer); err != nil {
				logger.Errorf("Error reinstalling %s, %v", pi.Name, err)
			}
		} else if !v {
			logger.Errorf("Verification failed for %s, reinstall or run verify again with the '-reinstall' flag.", pkg)
			exitCode = subcommands.ExitFailure
			continue
		}
		msg := fmt.Sprintf("Verification of %s completed", pkg)
		logger.Info(msg)
		fmt.Println(msg)
	}
	return exitCode
}
