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

package listrepos

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&listReposCmd{}, "repository management") }

type listReposCmd struct{}

func (*listReposCmd) Name() string     { return "listrepos" }
func (*listReposCmd) Synopsis() string { return "list repositories" }
func (*listReposCmd) Usage() string {
	return fmt.Sprintf("%s listrepos\n", filepath.Base(os.Args[0]))
}

// NoPositionals marks listrepos as a command that takes no positional arguments.
func (*listReposCmd) NoPositionals() {}

var _ cli.NoPositionals = (*listReposCmd)(nil)

func (cmd *listReposCmd) SetFlags(f *flag.FlagSet) {}

// AllowUnelevated reports that listrepos may always skip the admin check and
// the lock, because it only reads the repo files.
func (*listReposCmd) AllowUnelevated() bool { return true }

// LockFree marks listrepos as a command that runs without the admin check,
// the lock and the log file even when elevated, as it always has.
func (*listReposCmd) LockFree() {}

var _ cli.LockFree = (*listReposCmd)(nil)

func (cmd *listReposCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	// TestRefuseUnlessAllowedFirst requires this guard. It can't refuse
	// while AllowUnelevated returns true.
	if cli.RefuseUnlessAllowed(ctx, cmd) {
		logger.Error("Listing repositories requires administrator privileges.")
		return subcommands.ExitFailure
	}
	rfs, err := repo.ConfigFiles(settings.RepoDir())
	if err != nil {
		logger.Errorf("Failed to read repo configs: %v", err)
		return subcommands.ExitFailure
	}
	for _, rf := range rfs {
		fmt.Println(rf.Path + ":")
		for _, re := range rf.Entries {
			fmt.Printf("  %s: %s\n", re.Name, re.URL)
		}
	}
	return subcommands.ExitSuccess
}
