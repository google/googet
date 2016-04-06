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

	"github.com/google/googet/goolib"
	"github.com/google/logger"
	"github.com/google/subcommands"
	"golang.org/x/net/context"
)

type cleanCmd struct{}

func (*cleanCmd) Name() string     { return "clean" }
func (*cleanCmd) Synopsis() string { return "clean the cache directory" }
func (*cleanCmd) Usage() string {
	return fmt.Sprintf("%s clean\n", filepath.Base(os.Args[0]))
}

func (cmd *cleanCmd) SetFlags(f *flag.FlagSet) {}

func (cmd *cleanCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	fmt.Println("Removing all files and directories in cachedir that dont correspond to a currently installed package.")
	state, err := readState(filepath.Join(rootDir, stateFile))
	if err != nil {
		logger.Fatal(err)
	}
	var il []string
	for _, pkg := range *state {
		il = append(il, pkg.UnpackDir)
	}
	files, err := filepath.Glob(filepath.Join(rootDir, cacheDir, "*"))
	if err != nil {
		logger.Fatal(err)
	}
	for _, file := range files {
		if !goolib.ContainsString(file, il) {
			if err := os.RemoveAll(file); err != nil {
				logger.Error(err)
			}
		}
	}

	return subcommands.ExitSuccess
}
