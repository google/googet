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
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/subcommands"
)

// NoPositionals is implemented by commands that take no positional
// arguments. CheckArgs rejects any positional argument for these commands.
type NoPositionals interface {
	NoPositionals()
}

// CheckArgs reports a usage error in args, the arguments that follow the
// subcommand name, for cmd.
//
// The flag package stops parsing at the first positional argument, so a flag
// written after another argument, as in "install pkg -dry_run", would
// otherwise be taken as another positional argument. CheckArgs rejects any
// argument that starts with "-" after the first positional argument, unless
// the positional arguments are preceded by a "--" terminator. Commands that
// implement NoPositionals may not have any positional argument.
//
// CheckArgs parses args the same way subcommands.Commander.Execute does. If
// that parse fails, CheckArgs returns nil so that Execute reports the parse
// error and prints the usage. CheckArgs calls cmd.SetFlags, which may store
// defaults and parsed values in cmd; Execute calls SetFlags again before it
// parses, so this has no lasting effect.
func CheckArgs(cmd subcommands.Command, args []string) error {
	pos, ok := parseArgs(cmd, args)
	if !ok {
		return nil
	}
	if _, ok := cmd.(NoPositionals); ok {
		if len(pos) > 0 {
			return fmt.Errorf("%s takes no arguments, got %q", cmd.Name(), pos[0])
		}
		return nil
	}
	if len(pos) < 2 || terminated(cmd, args, len(args)-len(pos)) {
		return nil
	}
	for _, a := range pos[1:] {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("argument %q after %q looks like a flag; flags must come before other arguments (use \"--\" before arguments that start with \"-\")", a, pos[0])
		}
	}
	return nil
}

// parseArgs parses args with a fresh flag set filled by cmd.SetFlags and
// returns the remaining positional arguments. It reports false if the parse
// fails.
func parseArgs(cmd subcommands.Command, args []string) ([]string, bool) {
	f := flag.NewFlagSet(cmd.Name(), flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.Usage = func() {}
	cmd.SetFlags(f)
	if err := f.Parse(args); err != nil {
		return nil, false
	}
	return f.Args(), true
}

// terminated reports whether the flag package stopped parsing args because of
// a "--" terminator, given that it consumed the first n arguments as flags.
// The flag package consumes the terminator, so it can only be the last
// consumed argument. That argument can also be the value of a flag, as in
// "-sources -- pkg"; it's a terminator only if the arguments before it parse
// as flags on their own.
func terminated(cmd subcommands.Command, args []string, n int) bool {
	if n == 0 || args[n-1] != "--" {
		return false
	}
	pos, ok := parseArgs(cmd, args[:n-1])
	return ok && len(pos) == 0
}
