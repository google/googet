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
