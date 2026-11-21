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

// The googet binary is the client for the GoGet packaging system, it performs the listing,
// getting, installing and removing functions on client machines.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/progress"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
	"github.com/google/subcommands"

	_ "github.com/google/googet/v2/cli/addrepo"
	_ "github.com/google/googet/v2/cli/available"
	_ "github.com/google/googet/v2/cli/check"
	_ "github.com/google/googet/v2/cli/clean"
	_ "github.com/google/googet/v2/cli/download"
	_ "github.com/google/googet/v2/cli/install"
	_ "github.com/google/googet/v2/cli/installed"
	_ "github.com/google/googet/v2/cli/latest"
	_ "github.com/google/googet/v2/cli/listrepos"
	_ "github.com/google/googet/v2/cli/remove"
	_ "github.com/google/googet/v2/cli/rmrepo"
	_ "github.com/google/googet/v2/cli/update"
	_ "github.com/google/googet/v2/cli/verify"
)

const (
	// envVar is the environment variable which stores the googet root directory.
	envVar = "GooGetRoot"
	// logSize is the max allowed size of the log file.
	logSize = 10 * 1024 * 1024
)

var (
	// version is the googet version, set via linkopts.
	version string
	// Optional function to handle flag parsing. If unset, we use flag.Parse.
	flagParse func()
	// isAdmin returns an error if the process lacks administrator rights. It
	// is a variable so that tests can replace it.
	isAdmin = system.IsAdmin
	// obtainLock takes the googet lock file. It is a variable so that tests
	// can replace it.
	obtainLock = system.ObtainLock
)

func rotateLog(logPath string, ls int64) error {
	fi, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Size() < ls {
		return nil
	}
	oldLog := logPath + ".old"
	if err := os.Rename(logPath, oldLog); err != nil {
		return fmt.Errorf("error moving log file: %v", err)
	}
	return nil
}

func main() {
	os.Exit(run(context.Background()))
}

func run(ctx context.Context) int {
	rootDir := flag.String("root", os.Getenv(envVar), "googet root directory")
	noConfirm := flag.Bool("noconfirm", false, "skip confirmation")
	verbose := flag.Bool("verbose", false, "print info level logs to stdout")
	systemLog := flag.Bool("system_log", true, "log to Linux Syslog or Windows Event Log")
	progressFlag := flag.Bool("progress", true, "show a download progress bar and install spinner when stderr is a terminal (never with -verbose); overrides progress in googet.conf")
	showVer := flag.Bool("version", false, "display GooGet version and exit")

	if flagParse != nil {
		flagParse()
	} else {
		flag.Parse()
	}

	if *showVer {
		fmt.Println("GooGet version:", version)
		return 0
	}

	cmdr := subcommands.DefaultCommander
	cmdr.Register(cmdr.FlagsCommand(), "")
	cmdr.Register(cmdr.CommandsCommand(), "")
	cmdr.Register(cmdr.HelpCommand(), "")
	cmdr.ImportantFlag("verbose")
	cmdr.ImportantFlag("noconfirm")
	cmdr.ImportantFlag("progress")

	// These commands may execute without a lock and before any initialization.
	cmdName := flag.Arg(0) // empty string if no args
	switch cmdName {
	case "", "help", "commands", "flags":
		return int(cmdr.Execute(ctx))
	}

	// Decide how to run before anything is touched. Usage-only runs never
	// reach a command's Execute, so they need no root, admin check, lock or
	// log; Execute prints the usage or the parse error.
	class, cmd, argsErr := cli.Classify(cmdr, flag.Args())
	if class == cli.UsageOnly {
		if argsErr != nil {
			fmt.Fprintf(cmdr.Error, "%s: %v\n", cmdName, argsErr)
			cmdr.ExplainCommand(cmdr.Error, cmd)
			return int(subcommands.ExitUsageError)
		}
		return int(cmdr.Execute(ctx))
	}

	if *rootDir == "" {
		logger.Errorf("The environment variable %q is not defined and no '-root' flag passed", envVar)
		return 1
	}
	settings.Initialize(*rootDir, !*noConfirm)

	initProgress := func() {
		progressSet := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "progress" {
				progressSet = true
			}
		})
		progress.Init(wantProgress(settings.Progress, progressSet, *progressFlag, *verbose))
	}

	// Unelevated runs must not create anything under the root, so they run
	// only if the root and the database already exist. Otherwise they take
	// the privileged path below, which creates them after the admin check.
	// A LockFree command runs unelevated even in an elevated process. Any
	// other command runs unelevated only if the admin check fails, so an
	// elevated run keeps the lock, the log and the cache refresh.
	rootExists := isDir(*rootDir)
	dbFile := settings.DBFile()
	canUnelevated := class == cli.UnelevatedRun && rootExists && googetdb.Exists(dbFile)
	_, lockFree := cmd.(cli.LockFree)
	runUnelevated := func() int {
		// No lock, no googet.log and no system log, which on Windows would
		// register an event source under HKLM. Errors go to stderr; with
		// -verbose, info logs go to stdout. The context isn't marked
		// privileged, so the command refuses anything its AllowUnelevated
		// doesn't allow.
		logger.Init("GooGet", *verbose, false, io.Discard)
		defer logger.Close()
		initProgress()
		return int(cmdr.Execute(ctx))
	}
	if canUnelevated && lockFree {
		return runUnelevated()
	}
	adminErr := isAdmin()
	if canUnelevated && adminErr != nil {
		return runUnelevated()
	}
	if err := adminErr; err != nil {
		if !rootExists {
			err = fmt.Errorf("%v; googet root %q doesn't exist, run googet elevated first", err, *rootDir)
		}
		logger.Errorf("Failed admin check: %v", err)
		return 1
	}
	// The root is created only after the admin check, so an unelevated run
	// never creates it. It is created before the lock so that it gets this
	// mode rather than the one obtainLock uses for the lock file's directory.
	if err := os.MkdirAll(*rootDir, 0774); err != nil {
		logger.Errorf("Unable to create root directory: %v", err)
		return 1
	}
	cleanup, err := obtainLock(settings.LockFile(), settings.LockFileMaxAge)
	if err != nil {
		logger.Errorf("Failed to obtain lock: %v", err)
		return 1
	}
	if cleanup != nil {
		defer cleanup()
	}
	ctx = cli.WithPrivileged(ctx)

	logFile := settings.LogFile()
	if err := rotateLog(logFile, logSize); err != nil {
		logger.Errorf("Failed to rotate log file %q: %v", logFile, err)
		return 1
	}
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
	if err != nil {
		logger.Errorf("failed to open log file %q: %v", logFile, err)
		return 1
	}
	defer lf.Close()
	logger.Init("GooGet", *verbose, *systemLog, lf)
	defer logger.Close()

	initProgress()

	if err := googetdb.CreateIfMissing(dbFile); err != nil {
		logger.Errorf("Unable to create initial db file; if db is not created, run again as admin: %v", err)
		return 1
	}
	if err := os.MkdirAll(settings.CacheDir(), 0774); err != nil {
		logger.Errorf("Unable to create cache directory: %v", err)
		return 1
	}
	if err := os.MkdirAll(settings.RepoDir(), 0774); err != nil {
		logger.Errorf("Unable to create repo directory: %v", err)
		return 1
	}
	return int(cmdr.Execute(ctx))
}

// isDir reports whether path exists and is a directory. Any error, including
// a permission error, counts as not existing.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// wantProgress reports whether progress output should be rendered, given the
// progress setting from googet.conf, whether -progress was set explicitly and
// its value, and -verbose. Progress is still subject to the terminal checks in
// progress.Init. An explicit -progress, true or false, overrides the config;
// -verbose always disables progress because it interleaves INFO logs on
// stdout, which would corrupt a redrawn line.
func wantProgress(conf, flagSet, flagVal, verbose bool) bool {
	if verbose {
		return false
	}
	if flagSet {
		return flagVal
	}
	return conf
}
