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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/oswrap"
	"github.com/google/logger"
	"github.com/google/subcommands"
	"golang.org/x/net/context"
)

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
	var name string
	switch f.NArg() {
	case 0:
		fmt.Fprintln(os.Stderr, "Not enough arguments")
		f.Usage()
		return subcommands.ExitUsageError
	case 1:
		name = f.Arg(0)
	default:
		fmt.Fprintln(os.Stderr, "Excessive arguments")
		f.Usage()
		return subcommands.ExitUsageError
	}

	repoEntries, err := repos(filepath.Join(rootDir, repoDir))
	if err != nil {
		logger.Fatal(err)
	}

	var repoPath string
	for _, re := range repoEntries {
		if strings.ToLower(re.Name) == strings.ToLower(name) {
			repoPath = re.fileName
		}
	}

	if repoPath == "" {
		fmt.Fprintf(os.Stderr, "Repo %q not found, nothing to remove.\n", name)
		return subcommands.ExitUsageError
	}

	urf, err := unmarshalRepoFile(repoPath)
	if err != nil {
		logger.Fatal(err)
	}

	var rfs []repoFile
	for i, rf := range urf {
		if strings.ToLower(rf.Name) != strings.ToLower(name) {
			rfs = append(rfs, rf)
		}
	}

	if len(rfs) > 0 {
		if err := writeRepoFile(repoPath, rfs); err != nil {
			logger.Fatal(err)
		}
		fmt.Printf("Removed repo %q from repo file %s.\n", name, repoPath)
		return subcommands.ExitSuccess
	}

	if err := oswrap.Remove(repoPath); err != nil {
		logger.Fatal(err)
	}
	fmt.Printf("Removed repo %q and repo file %s.\n", name, repoPath)
	return subcommands.ExitSuccess
}
