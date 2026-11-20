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
	"os"

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
	// logSize is the size at which the log file is rotated.
	logSize = 25 * 1024 * 1024
	// logKeep is the number of rotated log files to keep.
	logKeep = 5
)

var (
	// version is the googet version, set via linkopts.
	version string
	// Optional function to handle flag parsing. If unset, we use flag.Parse.
	flagParse func()

	// rotateRename and rotateRemove are used by rotateLog so tests can
	// simulate files that can't be moved or deleted, such as files locked
	// on Windows.
	rotateRename = os.Rename
	rotateRemove = os.Remove
)

// logGeneration returns the name of rotated log generation n of logPath. The
// newest generation, 1, is logPath.old and older ones are logPath.old.2 and up.
func logGeneration(logPath string, n int) string {
	if n <= 1 {
		return logPath + ".old"
	}
	return fmt.Sprintf("%s.old.%d", logPath, n)
}

// rotateLog moves logPath to logPath.old once it reaches maxSize bytes, and
// keeps up to keep rotated generations (at least one). Each older generation
// moves up one place and the oldest is deleted. Rotation is best effort: it
// skips missing generations and continues past files that can't be moved or
// deleted, such as files locked on Windows. If an older generation can't be
// moved, a neighboring generation may be overwritten instead; which one
// depends on whether the platform can rename over a locked file. It never
// moves the live log onto a logPath.old that is still in place, so if
// logPath.old can't be moved the live log isn't rotated and the caller keeps
// appending to it. rotateLog runs before the logger is set up, so it returns
// any problems as warnings for the caller to log instead of logging them
// itself.
func rotateLog(logPath string, maxSize int64, keep int) []error {
	fi, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{fmt.Errorf("unable to check log file size, not rotating: %v", err)}
	}
	if fi.Size() < maxSize {
		return nil
	}
	if keep < 1 {
		keep = 1
	}

	var warnings []error
	oldest := logGeneration(logPath, keep)
	if err := rotateRemove(oldest); err != nil && !os.IsNotExist(err) {
		warnings = append(warnings, fmt.Errorf("unable to delete old log file %q: %v", oldest, err))
	}
	for n := keep - 1; n >= 1; n-- {
		src, dst := logGeneration(logPath, n), logGeneration(logPath, n+1)
		if err := rotateRename(src, dst); err != nil && !os.IsNotExist(err) {
			warnings = append(warnings, fmt.Errorf("unable to move old log file %q to %q: %v", src, dst, err))
		}
	}

	newest := logGeneration(logPath, 1)
	if _, err := os.Lstat(newest); !os.IsNotExist(err) {
		return append(warnings, fmt.Errorf("%q is still in place, not rotating log file %q", newest, logPath))
	}
	if err := rotateRename(logPath, newest); err != nil {
		warnings = append(warnings, fmt.Errorf("unable to move log file %q to %q, appending to it instead: %v", logPath, newest, err))
	}
	return warnings
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

	if *rootDir == "" {
		logger.Errorf("The environment variable %q is not defined and no '-root' flag passed", envVar)
		return 1
	}
	if err := os.MkdirAll(*rootDir, 0774); err != nil {
		logger.Errorf("Unable to create root directory: %v", err)
		return 1
	}
	settings.Initialize(*rootDir, !*noConfirm)

	// "googet listrepos" may execute without a lock after the root directory and
	// settings are initialized.
	if cmdName == "listrepos" {
		return (int(cmdr.Execute(context.Background())))
	}

	dbFile := settings.DBFile()

	// "googet installed" is allowed to execute without a lock if the googet
	// database has already been created.
	if googetdb.Exists(dbFile) && cmdName == "installed" {
		return int(cmdr.Execute(ctx))
	}

	if err := system.IsAdmin(); err != nil {
		logger.Errorf("Failed admin check: %v", err)
		return 1
	}
	cleanup, err := system.ObtainLock(settings.LockFile(), settings.LockFileMaxAge)
	if err != nil {
		logger.Errorf("Failed to obtain lock: %v", err)
		return 1
	}
	if cleanup != nil {
		defer cleanup()
	}

	logFile := settings.LogFile()
	rotateWarnings := rotateLog(logFile, logSize, logKeep)
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
	if err != nil {
		logger.Errorf("failed to open log file %q: %v", logFile, err)
		return 1
	}
	defer lf.Close()
	logger.Init("GooGet", *verbose, *systemLog, lf)
	defer logger.Close()
	for _, w := range rotateWarnings {
		logger.Warningf("Log rotation: %v", w)
	}

	progressSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "progress" {
			progressSet = true
		}
	})
	progress.Init(wantProgress(settings.Progress, progressSet, *progressFlag, *verbose))

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
