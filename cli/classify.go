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

	"github.com/google/subcommands"
)

// AllowUnelevated is implemented by commands that may run in an unelevated
// process, without the lock and the log file, for some or all of their flag
// values. Unless the command implements LockFree, the googet binary still
// runs the admin check first and takes the privileged path when it passes.
//
// AllowUnelevated is called after the command's SetFlags has registered its
// flags on a fresh flag set and that flag set has parsed the command's
// arguments. It must depend only on the command's parsed flag fields, must do
// no I/O, and must agree with Execute: Execute must not create, write or
// delete anything under the googet root, or run package code, when
// AllowUnelevated reports true.
type AllowUnelevated interface {
	AllowUnelevated() bool
}

// LockFree is implemented by commands that run without the admin check, the
// lock and the log file whenever their AllowUnelevated allows the parsed
// flags, even in an elevated process. They only read the database or the repo
// files, never need the repo index cache or the log, and googet has always
// run them this way.
//
// Other AllowUnelevated commands run that way only when the admin check
// fails, so an elevated run of them keeps the lock, the log and the cache
// refresh.
//
// A LockFree command never gets the privileged context marker, so OpenDB
// opens the database read-only for it even in an elevated process. A
// read-only connection can't roll back a hot journal left by a writer that
// crashed, so a LockFree command gets an error reading the database until a
// privileged command opens it read-write and rolls the journal back. That is
// accepted to keep LockFree commands free of the lock.
type LockFree interface {
	AllowUnelevated
	// LockFree marks the command. It is never called.
	LockFree()
}

// Class is the kind of run that Classify decides a command line needs.
type Class int

const (
	// Privileged runs need the admin check, the lock and the log file. It is
	// the default.
	Privileged Class = iota
	// UsageOnly runs never reach a command's Execute: the command is unknown,
	// its arguments don't parse (including -h and -help), or CheckArgs
	// rejects them. Only usage or an error is printed.
	UsageOnly
	// UnelevatedRun runs may skip the admin check, the lock and the log file,
	// because the command implements AllowUnelevated and allows the parsed
	// flags. The googet binary skips them only if the root and the database
	// exist and either the command implements LockFree or the admin check
	// fails.
	UnelevatedRun
)

// String returns the name of c.
func (c Class) String() string {
	switch c {
	case Privileged:
		return "Privileged"
	case UsageOnly:
		return "UsageOnly"
	case UnelevatedRun:
		return "UnelevatedRun"
	}
	return "Class(unknown)"
}

// Classify decides what kind of run args needs, where args are the arguments
// left after the top-level flags: the subcommand name followed by its
// arguments. It does no I/O. It also returns the command that args name, or
// nil if there is none, and the error from CheckArgs, if any, so the caller
// can report it without looking the command up again.
//
// Classify looks the name up and parses the subcommand's arguments exactly as
// subcommands.Commander.Execute does: the first command whose Name equals
// args[0], in the order Execute searches, and a fresh ContinueOnError flag set
// filled by that command's SetFlags. Like Execute, it calls SetFlags on the
// registered command, which stores flag values in it; Execute calls SetFlags
// again before it parses, so this has no lasting effect.
//
// Classify fails closed. It returns UsageOnly only when Execute would not call
// the command's Execute, or when CheckArgs reports an error. It returns
// UnelevatedRun only when exactly one command has the name, it implements
// AllowUnelevated, and AllowUnelevated reports true for the parsed flags.
// Everything else is Privileged, including aliases, which don't implement
// AllowUnelevated.
func Classify(cmdr *subcommands.Commander, args []string) (Class, subcommands.Command, error) {
	if len(args) == 0 {
		return UsageOnly, nil, nil
	}
	// VisitCommands sorts the groups in place before visiting them, so
	// Execute, which walks the same groups, then searches in this order too.
	var cmd subcommands.Command
	matches := 0
	cmdr.VisitCommands(func(_ *subcommands.CommandGroup, c subcommands.Command) {
		if c.Name() != args[0] {
			return
		}
		if cmd == nil {
			cmd = c
		}
		matches++
	})
	if cmd == nil {
		return UsageOnly, nil, nil
	}
	if err := CheckArgs(cmd, args[1:]); err != nil {
		return UsageOnly, cmd, err
	}
	// Parse last, so the flag fields that AllowUnelevated reads hold the
	// values from parsing all of args[1:].
	if _, ok := parseArgs(cmd, args[1:]); !ok {
		return UsageOnly, cmd, nil
	}
	if matches > 1 {
		return Privileged, cmd, nil
	}
	if a, ok := cmd.(AllowUnelevated); ok && a.AllowUnelevated() {
		return UnelevatedRun, cmd, nil
	}
	return Privileged, cmd, nil
}

// RefuseUnlessAllowed reports whether a command's Execute must refuse to run
// because ctx isn't marked privileged and cmd doesn't allow its parsed flags
// to run unelevated. Every command that implements AllowUnelevated calls it
// as the first statement of Execute, so the command never relies on run() to
// have classified it correctly.
func RefuseUnlessAllowed(ctx context.Context, cmd AllowUnelevated) bool {
	return !IsPrivileged(ctx) && !cmd.AllowUnelevated()
}

// RefuseUnlessPrivileged reports whether a command's Execute must refuse to
// run because ctx isn't marked privileged. Every command that doesn't
// implement AllowUnelevated calls it as the first statement of Execute, so
// the command never relies on run() to have classified it correctly.
func RefuseUnlessPrivileged(ctx context.Context) bool {
	return !IsPrivileged(ctx)
}
