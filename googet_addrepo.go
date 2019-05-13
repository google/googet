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

	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

type addRepoCmd struct {
	file string
}

func (*addRepoCmd) Name() string     { return "addrepo" }
func (*addRepoCmd) Synopsis() string { return "add repository" }
func (*addRepoCmd) Usage() string {
	return fmt.Sprintf(`%s addrepo [-file] <name> <url>:
	Add repository to GooGet's repository list. 
	If -file is not set 'name.repo' will be used for the file name 
	overwriting any existing file with than name. 
	If -file is set the specified repo will be appended to that repo file, 
	creating it if it does not exist.
`, filepath.Base(os.Args[0]))
}

func (cmd *addRepoCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&cmd.file, "file", "", "repo file to add this repository to")
}

func (cmd *addRepoCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	var name, url string
	switch f.NArg() {
	case 0, 1:
		fmt.Fprintln(os.Stderr, "Not enough arguments")
		f.Usage()
		return subcommands.ExitUsageError
	case 2:
		name = f.Arg(0)
		url = f.Arg(1)
	default:
		fmt.Fprintln(os.Stderr, "Excessive arguments")
		f.Usage()
		return subcommands.ExitUsageError
	}

	if cmd.file == "" {
		cmd.file = name + ".repo"
	} else {
		if !strings.HasSuffix(cmd.file, ".repo") {
			fmt.Fprintln(os.Stderr, "Repo file name must end in '.repo'")
			return subcommands.ExitUsageError
		}
	}

	repoPath := filepath.Join(rootDir, repoDir, cmd.file)

	if _, err := oswrap.Stat(repoPath); err != nil && os.IsNotExist(err) {
		re := repoEntry{Name: name, URL: url}
		if err := writeRepoFile(repoFile{repoPath, []repoEntry{re}}); err != nil {
			logger.Fatal(err)
		}
		fmt.Printf("Wrote repo file %s with content:\n  Name: %s\n  URL: %s\n", repoPath, re.Name, re.URL)
		return subcommands.ExitSuccess
	}

	rf, err := unmarshalRepoFile(repoPath)
	if err != nil {
		logger.Fatal(err)
	}

	var res []repoEntry
	for _, re := range rf.repoEntries {
		if re.Name != name && re.URL != url {
			res = append(res, re)
		}
	}

	re := repoEntry{Name: name, URL: url}
	res = append(res, re)
	rf = repoFile{rf.fileName, res}

	if err := writeRepoFile(rf); err != nil {
		logger.Fatal(err)
	}
	fmt.Printf("Appended to repo file %s with the following content:\n  Name: %s\n  URL: %s\n", repoPath, re.Name, re.URL)

	return subcommands.ExitSuccess
}
