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

package rmrepo

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&rmRepoCmd{}, "repository management") }

type rmRepoCmd struct{}

func (*rmRepoCmd) Name() string     { return "rmrepo" }
func (*rmRepoCmd) Synopsis() string { return "remove repository" }
func (*rmRepoCmd) Usage() string {
	return fmt.Sprintf(`%s rmrepo <name>:
	Removes the named repository from GooGet's repository list.
`, filepath.Base(os.Args[0]))
}

func (cmd *rmRepoCmd) SetFlags(f *flag.FlagSet) {}

func (cmd *rmRepoCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if got, want := f.NArg(), 1; got != want {
		fmt.Fprintf(os.Stderr, "Wrong number of arguments: got %v, want %v\n", got, want)
		f.Usage()
		return subcommands.ExitUsageError
	}

	name := f.Arg(0)
	changed, err := repo.RemoveEntryFromFiles(name, settings.RepoDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to remove %q entries: %v\n", name, err)
		return subcommands.ExitFailure
	}

	if len(changed) == 0 {
		fmt.Fprintf(os.Stderr, "Entry %q not found in any files\n", name)
	} else {
		fmt.Fprintf(os.Stderr, "Removed %q from files: %v\n", name, strings.Join(changed, ", "))
	}
	return subcommands.ExitSuccess
}
