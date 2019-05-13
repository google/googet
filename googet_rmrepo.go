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

	rfs, err := repos(filepath.Join(rootDir, repoDir))
	if err != nil {
		logger.Fatal(err)
	}

	var foundRepo repoFile
	for _, rf := range rfs {
		for _, re := range rf.repoEntries {
			if strings.ToLower(re.Name) == strings.ToLower(name) {
				foundRepo = rf
				break
			}
		}
	}

	if foundRepo.fileName == "" {
		fmt.Fprintf(os.Stderr, "Repo %q not found, nothing to remove.\n", name)
		return subcommands.ExitUsageError
	}

	var res []repoEntry
	for _, re := range foundRepo.repoEntries {
		if strings.ToLower(re.Name) != strings.ToLower(name) {
			res = append(res, re)
		}
	}

	if len(res) > 0 {
		if err := writeRepoFile(repoFile{foundRepo.fileName, res}); err != nil {
			logger.Fatal(err)
		}
		fmt.Printf("Removed repo %q from repo file %s.\n", name, foundRepo.fileName)
		return subcommands.ExitSuccess
	}

	if err := oswrap.Remove(foundRepo.fileName); err != nil {
		logger.Fatal(err)
	}
	fmt.Printf("Removed repo %q and repo file %s.\n", name, foundRepo.fileName)
	return subcommands.ExitSuccess
}
