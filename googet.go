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
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
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

func confirmation(msg string) bool {
	var c string
	fmt.Print(msg + " (y/N): ")
	fmt.Scanln(&c)
	c = strings.ToLower(c)
	return c == "y" || c == "yes"
}

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

var deferredFuncs []func()

func runDeferredFuncs() {
	for _, f := range deferredFuncs {
		f()
	}
}

func obtainLock(lockFile string) error {
	err := os.MkdirAll(filepath.Dir(lockFile), 0755)
	if err != nil && !os.IsExist(err) {
		return err
	}

	f, err := os.OpenFile(lockFile, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil && !os.IsExist(err) {
		return err
	}

	c := make(chan error)
	go func() {
		c <- lock(f)
	}()

	ticker := time.NewTicker(5 * time.Second)
	// 90% of all GooGet runs happen in < 60s, we wait 70s.
	for i := 1; i < 15; i++ {
		select {
		case err := <-c:
			if err != nil {
				return err
			}
			return nil
		case <-ticker.C:
			fmt.Fprintln(os.Stdout, "GooGet lock already held, waiting...")
		}
	}
	return errors.New("timed out waiting for lock")
}

func main() {
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
		os.Exit(0)
	}

	cmdr := subcommands.NewCommander(flag.CommandLine, "googet")
	cmdr.Register(cmdr.FlagsCommand(), "")
	cmdr.Register(cmdr.CommandsCommand(), "")
	cmdr.Register(cmdr.HelpCommand(), "")
	cmdr.Register(&installCmd{}, "package management")
	cmdr.Register(&downloadCmd{}, "package management")
	cmdr.Register(&removeCmd{}, "package management")
	cmdr.Register(&updateCmd{}, "package management")
	cmdr.Register(&verifyCmd{}, "package management")
	cmdr.Register(&installedCmd{}, "package query")
	cmdr.Register(&latestCmd{}, "package query")
	cmdr.Register(&availableCmd{}, "package query")
	cmdr.Register(&checkCmd{}, "package query")
	cmdr.Register(&listReposCmd{}, "repository management")
	cmdr.Register(&addRepoCmd{}, "repository management")
	cmdr.Register(&rmRepoCmd{}, "repository management")
	cmdr.Register(&cleanCmd{}, "")

	cmdr.ImportantFlag("verbose")
	cmdr.ImportantFlag("noconfirm")

	// These commands may execute without a lock and before any initialization.
	cmdName := flag.Arg(0) // empty string if no args
	switch cmdName {
	case "", "help", "commands", "flags":
		os.Exit(int(cmdr.Execute(context.Background())))
	}

	if *rootDir == "" {
		logger.Fatalf("The environment variable %q not defined and no '-root' flag passed.", envVar)
	}
	if err := os.MkdirAll(*rootDir, 0774); err != nil {
		logger.Fatalln("Error setting up root directory:", err)
	}
	settings.Initialize(*rootDir, !*noConfirm)

	// "googet listrepos" may execute without a lock after the root directory and
	// settings are initialized.
	if cmdName == "listrepos" {
		os.Exit(int(cmdr.Execute(context.Background())))
	}

	dbFile := settings.DBFile()

	// "googet installed" is allowed to execute without a lock if the googet
	// database has already been created.
	if googetdb.Exists(dbFile) && cmdName == "installed" {
		os.Exit(int(cmdr.Execute(context.Background())))
	}

	if err := obtainLock(settings.LockFile()); err != nil {
		logger.Fatalf("Cannot obtain GooGet lock, you may need to run with admin rights, error: %v", err)
	}

	logPath := settings.LogFile()
	if err := rotateLog(logPath, logSize); err != nil {
		logger.Error(err)
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
	if err != nil {
		runDeferredFuncs()
		logger.Fatalln("Failed to open log file:", err)
	}
	deferredFuncs = append(deferredFuncs, func() { lf.Close() })

	logger.Init("GooGet", *verbose, *systemLog, lf)

	if err := googetdb.CreateIfMissing(dbFile); err != nil {
		runDeferredFuncs()
		logger.Fatalf("Error creating initial db file. If db is not created, run again as admin: %v", err)
	}
	if err := os.MkdirAll(settings.CacheDir(), 0774); err != nil {
		runDeferredFuncs()
		logger.Fatalf("Error setting up cache directory: %v", err)
	}
	if err := os.MkdirAll(settings.RepoDir(), 0774); err != nil {
		runDeferredFuncs()
		logger.Fatalf("Error setting up repo directory: %v", err)
	}
	es := cmdr.Execute(context.Background())
	runDeferredFuncs()
	os.Exit(int(es))
}
