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
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
	"gopkg.in/yaml.v3"
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

type repoFile struct {
	fileName    string
	repoEntries []repoEntry
}

type repoEntry struct {
	Name     string
	URL      string
	UseOAuth bool
	Priority priority.Value `yaml:",omitempty"`
}

// UnmarshalYAML provides custom unmarshalling for repoEntry objects.
func (r *repoEntry) UnmarshalYAML(unmarshal func(any) error) error {
	var u map[string]string
	if err := unmarshal(&u); err != nil {
		return err
	}
	for k, v := range u {
		switch key := strings.ToLower(k); key {
		case "name":
			r.Name = v
		case "url":
			r.URL = v
		case "useoauth":
			r.UseOAuth = strings.ToLower(v) == "true"
		case "priority":
			var err error
			r.Priority, err = priority.FromString(v)
			if err != nil {
				return fmt.Errorf("invalid priority: %v", v)
			}
		}
	}
	if r.URL == "" {
		return fmt.Errorf("repo entry missing url: %+v", u)
	}
	return nil
}

func writeRepoFile(rf repoFile) error {
	d, err := yaml.Marshal(rf.repoEntries)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(rf.fileName, d, 0664)
}

func unmarshalRepoFile(p string) (repoFile, error) {
	b, err := ioutil.ReadFile(p)
	if err != nil {
		return repoFile{}, err
	}

	// Don't try to unmarshal files with no YAML content
	var yml bool
	lns := strings.Split(string(b), "\n")
	for _, ln := range lns {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "#") && ln != "" {
			yml = true
			break
		}
	}
	if !yml {
		return repoFile{}, nil
	}

	// Both repoFile and []repoFile are valid for backwards compatibility.
	var re repoEntry
	if err := yaml.Unmarshal(b, &re); err == nil && re.URL != "" {
		return repoFile{fileName: p, repoEntries: []repoEntry{re}}, nil
	}

	var res []repoEntry
	if err := yaml.Unmarshal(b, &res); err != nil {
		return repoFile{}, err
	}
	return repoFile{fileName: p, repoEntries: res}, nil
}

// validateRepoURL uses the global allowUnsafeURL to determine if u should be checked for https or
// GCS status.
func validateRepoURL(u string) bool {
	if settings.AllowUnsafeURL {
		return true
	}
	gcs, _, _ := goolib.SplitGCSUrl(u)
	parsed, err := url.Parse(u)
	if err != nil {
		logger.Errorf("Failed to parse URL '%s', skipping repo", u)
		return false
	}
	if parsed.Scheme != "https" && !gcs {
		logger.Errorf("%s will not be used as a repository, only https and Google Cloud Storage endpoints will be used unless 'allowunsafeurl' is set to 'true' in googet.conf", u)
		return false
	}
	return true
}

// repoList returns a deduped set of all repos listed in the repo config files contained in dir.
// The repos are mapped to priority values. If a repo config does not specify a priority, the repo
// is assigned the default priority value. If the same repo appears multiple times with different
// priority values, it is mapped to the highest seen priority value.
func repoList(dir string) (map[string]priority.Value, error) {
	rfs, err := repos(dir)
	if err != nil {
		return nil, err
	}
	result := make(map[string]priority.Value)
	for _, rf := range rfs {
		for _, re := range rf.repoEntries {
			u := re.URL
			if u == "" || !validateRepoURL(u) {
				continue
			}
			if re.UseOAuth {
				u = "oauth-" + u
			}
			p := re.Priority
			if p <= 0 {
				p = priority.Default
			}
			if q, ok := result[u]; !ok || p > q {
				result[u] = p
			}
		}
	}
	return result, nil
}

func repos(dir string) ([]repoFile, error) {
	fl, err := filepath.Glob(filepath.Join(dir, "*.repo"))
	if err != nil {
		return nil, err
	}
	var rfs []repoFile
	for _, f := range fl {
		rf, err := unmarshalRepoFile(f)
		if err != nil {
			logger.Error(err)
			continue
		}
		if rf.fileName != "" {
			rfs = append(rfs, rf)
		}
	}
	return rfs, nil
}

func buildSources(s string) (map[string]priority.Value, error) {
	if s == "" {
		return repoList(settings.RepoDir())
	}
	m := make(map[string]priority.Value)
	for _, src := range strings.Split(s, ",") {
		m[src] = priority.Default
	}
	return m, nil
}

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
