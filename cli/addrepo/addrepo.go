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

package addrepo

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&addRepoCmd{}, "repository management") }

type addRepoCmd struct {
	file     string
	priority string
}

func (*addRepoCmd) Name() string     { return "addrepo" }
func (*addRepoCmd) Synopsis() string { return "add repository" }
func (*addRepoCmd) Usage() string {
	return fmt.Sprintf(`%s addrepo [-file <repofile>] [-priority <value>] <name> <url>:
	Add repository to GooGet's repository list.
	If -file is not set 'name.repo' will be used for the file name
	overwriting any existing file with that name.
	If -file is set the specified repo will be appended to that repo file,
	creating it if it does not exist.
	If -priority is specified, the repo will be configured with this priority level.
`, filepath.Base(os.Args[0]))
}

func (cmd *addRepoCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&cmd.file, "file", "", "repo file to add this repository to")
	f.StringVar(&cmd.priority, "priority", "", "priority level assigned to repository")
}

func (cmd *addRepoCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if got, want := f.NArg(), 2; got != want {
		fmt.Fprintf(os.Stderr, "Wrong number of arguments: got %v, want %v\n", got, want)
		f.Usage()
		return subcommands.ExitUsageError
	}

	entry := repo.Entry{Name: f.Arg(0), URL: f.Arg(1)}

	if cmd.file == "" {
		cmd.file = entry.Name + ".repo"
	} else {
		if !strings.HasSuffix(cmd.file, ".repo") {
			fmt.Fprintln(os.Stderr, "Repo file name must end in '.repo'")
			return subcommands.ExitUsageError
		}
	}

	if cmd.priority != "" {
		var err error
		entry.Priority, err = priority.FromString(cmd.priority)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unrecognized priority value: %q\n", cmd.priority)
			return subcommands.ExitUsageError
		}
	}

	repoFile := filepath.Join(settings.RepoDir(), cmd.file)
	content, err := repo.AddEntryToFile(entry, repoFile)
	if err != nil {
		logger.Error(err)
		return subcommands.ExitFailure
	}

	fmt.Printf("Appended to repo file %s with the following content:\n%s\n", repoFile, content)
	return subcommands.ExitSuccess
}
