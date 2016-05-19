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

	//	"github.com/google/logger"
	"github.com/google/subcommands"
	"golang.org/x/net/context"
)

type addRepoCmd struct{}

func (*addRepoCmd) Name() string     { return "addrepo" }
func (*addRepoCmd) Synopsis() string { return "add repository" }
func (*addRepoCmd) Usage() string {
	return fmt.Sprintf("%s addrepo <name> <url>\n", filepath.Base(os.Args[0]))
}

func (cmd *addRepoCmd) SetFlags(f *flag.FlagSet) {}

func (cmd *addRepoCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {

	return subcommands.ExitSuccess
}
