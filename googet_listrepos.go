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

	"github.com/google/logger"
	"github.com/google/subcommands"
	"golang.org/x/net/context"
)

type listReposCmd struct{}

func (*listReposCmd) Name() string     { return "listrepos" }
func (*listReposCmd) Synopsis() string { return "list repositories" }
func (*listReposCmd) Usage() string {
	return fmt.Sprintf("%s listrepos\n", filepath.Base(os.Args[0]))
}

func (cmd *listReposCmd) SetFlags(f *flag.FlagSet) {}

func (cmd *listReposCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	rfs, err := repos(filepath.Join(rootDir, repoDir))
	if err != nil {
		logger.Fatal(err)
	}

	for _, rf := range rfs {
		fmt.Println(rf.fileName + ":")

		for _, re := range rf.repoEntries {
			fmt.Printf("  %s: %s\n", re.Name, re.URL)
		}
	}
	return subcommands.ExitSuccess
}
