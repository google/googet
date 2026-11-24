/*
Copyright 2026 Google Inc. All Rights Reserved.
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

package cli

import (
	"context"
	"flag"
	"strings"
	"testing"

	"github.com/google/subcommands"
)

// argsCmd is a command with a bool flag and a string flag, like install's
// -dry_run and -sources.
type argsCmd struct {
	dryRun  bool
	sources string
}

func (*argsCmd) Name() string     { return "fake" }
func (*argsCmd) Synopsis() string { return "fake command" }
func (*argsCmd) Usage() string    { return "fake [flags] <name>...\n" }
func (c *argsCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&c.dryRun, "dry_run", false, "dry run")
	f.StringVar(&c.sources, "sources", "", "sources")
}
func (*argsCmd) Execute(context.Context, *flag.FlagSet, ...any) subcommands.ExitStatus {
	return subcommands.ExitSuccess
}

// noPosCmd is argsCmd for a command that takes no positional arguments.
type noPosCmd struct{ argsCmd }

func (*noPosCmd) NoPositionals() {}

func TestCheckArgs(t *testing.T) {
	for _, tc := range []struct {
		desc    string
		noPos   bool
		args    []string
		wantErr string // A substring of the error; "" means no error.
	}{
		{desc: "no arguments", args: nil},
		{desc: "one package", args: []string{"pkg"}},
		{desc: "flag before packages", args: []string{"-dry_run", "pkg", "other"}},
		{desc: "double-dash flag before packages", args: []string{"--dry_run", "pkg"}},
		{desc: "flag after package", args: []string{"pkg", "-dry_run"}, wantErr: `"-dry_run" after "pkg"`},
		{desc: "flag after second package", args: []string{"-dry_run", "a", "b", "-sources=x"}, wantErr: `"-sources=x" after "a"`},
		{desc: "false bool then flag after package", args: []string{"-dry_run=false", "pkg", "-x"}, wantErr: `"-x"`},
		{desc: "terminator allows dash names", args: []string{"-dry_run", "--", "pkg", "-x"}},
		{desc: "terminator first", args: []string{"--", "pkg", "-x"}},
		{desc: "terminator alone", args: []string{"--"}},
		{desc: "dash name after terminator", args: []string{"--", "-pkg"}},
		{desc: "terminator as string flag value is not a terminator", args: []string{"-sources", "--", "pkg", "-x"}, wantErr: `"-x" after "pkg"`},
		{desc: "terminator after string flag value", args: []string{"-sources", "--", "--", "pkg", "-x"}},
		{desc: "second terminator is a package name", args: []string{"--", "--", "pkg"}},
		{desc: "terminator after package", args: []string{"pkg", "--"}, wantErr: `"--" after "pkg"`},
		{desc: "terminator after package then flag", args: []string{"pkg", "--", "-dry_run"}, wantErr: `"--" after "pkg"`},
		{desc: "lone dash is a package name", args: []string{"-"}},
		{desc: "second lone dash rejected", args: []string{"-", "-"}, wantErr: `"-" after "-"`},
		{desc: "unknown flag left to Execute", args: []string{"-bogus", "pkg", "-x"}},
		{desc: "help left to Execute", args: []string{"-h"}},
		{desc: "missing flag value left to Execute", args: []string{"-sources"}},
		{desc: "no positionals: none", noPos: true, args: nil},
		{desc: "no positionals: flags only", noPos: true, args: []string{"-dry_run", "-sources", "x"}},
		{desc: "no positionals: terminator alone", noPos: true, args: []string{"--"}},
		{desc: "no positionals: name", noPos: true, args: []string{"foo"}, wantErr: `fake takes no arguments, got "foo"`},
		{desc: "no positionals: name then flag", noPos: true, args: []string{"foo", "-dry_run"}, wantErr: `got "foo"`},
		{desc: "no positionals: flag then name", noPos: true, args: []string{"-dry_run", "foo"}, wantErr: `got "foo"`},
		{desc: "no positionals: name after terminator", noPos: true, args: []string{"--", "foo"}, wantErr: `got "foo"`},
		{desc: "no positionals: dash name after terminator", noPos: true, args: []string{"--", "-x"}, wantErr: `got "-x"`},
		{desc: "no positionals: parse error left to Execute", noPos: true, args: []string{"-bogus", "foo"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var cmd subcommands.Command = &argsCmd{}
			if tc.noPos {
				cmd = &noPosCmd{}
			}
			err := CheckArgs(cmd, tc.args)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("CheckArgs(%q) = %v, want nil", tc.args, err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("CheckArgs(%q) = nil, want an error containing %q", tc.args, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("CheckArgs(%q) = %q, want it to contain %q", tc.args, err, tc.wantErr)
			}
		})
	}
}
