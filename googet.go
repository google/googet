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
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-yaml/yaml"
	"github.com/google/googet/client"
	"github.com/google/googet/goolib"
	"github.com/google/googet/system"
	"github.com/google/logger"
	"github.com/google/subcommands"
	"github.com/olekukonko/tablewriter"
)

const (
	stateFile = "googet.state"
	confFile  = "googet.conf"
	logFile   = "googet.log"
	lockFile  = "googet.lock"
	cacheDir  = "cache"
	repoDir   = "repos"
	envVar    = "GooGetRoot"
	logSize   = 10 * 1024 * 1024
)

var (
	rootDir        string
	noConfirm      bool
	verbose        bool
	systemLog      bool
	showVer        bool
	version        string
	cacheLife      = 3 * time.Minute
	archs          []string
	proxyServer    string
	allowUnsafeURL bool
)

type packageMap map[string]string

// installedPackages returns a packagemap of all installed packages based on the
// googet state file given.
func installedPackages(state client.GooGetState) packageMap {
	pm := make(packageMap)
	for _, p := range state {
		pm[p.PackageSpec.Name+"."+p.PackageSpec.Arch] = p.PackageSpec.Version
	}
	return pm
}

type repoFile struct {
	fileName    string
	repoEntries []repoEntry
}

type repoEntry struct {
	Name string
	URL  string
}

// UnmarshalYAML provides custom unmarshalling for repoEntry objects.
func (r *repoEntry) UnmarshalYAML(unmarshal func(interface{}) error) error {
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

type conf struct {
	Archs          []string
	CacheLife      string
	ProxyServer    string
	AllowUnsafeURL bool
}

func unmarshalConfFile(p string) (*conf, error) {
	b, err := ioutil.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var cf conf
	return &cf, yaml.Unmarshal(b, &cf)
}

func repoList(dir string) ([]string, error) {
	rfs, err := repos(dir)
	if err != nil {
		return nil, err
	}
	var rl []string
	for _, rf := range rfs {
		for _, re := range rf.repoEntries {
			switch {
			case re.URL != "":
				rl = append(rl, re.URL)
			}
		}
	}

	if !allowUnsafeURL {
		var srl []string
		for _, r := range rl {
			isGCSURL, _, _ := goolib.SplitGCSUrl(r)
			parsed, err := url.Parse(r)
			if err != nil {
				logger.Errorf("Failed to parse URL '%s', skipping repo", r)
				continue
			}
			if parsed.Scheme != "https" && !isGCSURL {
				logger.Errorf("%s will not be used as a repository, only https and Google Cloud Storage endpoints will be used unless 'allowunsafeurl' is set to 'true' in googet.conf", r)
				continue
			}
			srl = append(srl, r)
		}
		return srl, nil
	}
	return rl, nil
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

func writeState(s *client.GooGetState, sf string) error {
	b, err := s.Marshal()
	if err != nil {
		return err
	}
	return ioutil.WriteFile(sf, b, 0664)
}

func readState(sf string) (*client.GooGetState, error) {
	b, err := ioutil.ReadFile(sf)
	if os.IsNotExist(err) {
		logger.Info("No state file found, assuming no packages installed.")
		return &client.GooGetState{}, nil
	}
	if err != nil {
		return nil, err
	}
	return client.UnmarshalState(b)
}

func buildSources(s string) ([]string, error) {
	if s != "" {
		srcs := strings.Split(s, ",")
		return srcs, nil
	}
	return repoList(filepath.Join(rootDir, repoDir))
}

func confirmation(msg string) bool {
	var c string
	fmt.Print(msg + " (y/N): ")
	fmt.Scanln(&c)
	c = strings.ToLower(c)
	return c == "y" || c == "yes"
}

func info(ps *goolib.PkgSpec, r string) {
	fmt.Println()

	pkgInfo := []struct {
		name, value string
	}{
		{"Name", ps.Name},
		{"Arch", ps.Arch},
		{"Version", ps.Version},
		{"Repo", path.Base(r)},
		{"Authors", ps.Authors},
		{"Owners", ps.Owners},
		{"Description", ps.Description},
		{"Dependencies", ""},
		{"ReleaseNotes", ""},
	}
	var w int
	for _, pi := range pkgInfo {
		if len(pi.name) > w {
			w = len(pi.name)
		}
	}
	wf := fmt.Sprintf("%%-%vs: %%s\n", w+1)

	for _, pi := range pkgInfo {
		if pi.name == "Dependencies" {
			var deps []string
			for p, v := range ps.PkgDependencies {
				deps = append(deps, p+" "+v)
			}
			if len(deps) == 0 {
				fmt.Printf(wf, pi.name, "None")
			} else {
				fmt.Printf(wf, pi.name, deps[0])
				for _, l := range deps[1:] {
					fmt.Printf(wf, "", l)
				}
			}
		} else if pi.name == "ReleaseNotes" && ps.ReleaseNotes != nil {
			sl, _ := tablewriter.WrapString(ps.ReleaseNotes[0], 76-w)
			fmt.Printf(wf, pi.name, sl[0])
			for _, l := range sl[1:] {
				fmt.Printf(wf, "", l)
			}
			for _, l := range ps.ReleaseNotes[1:] {
				sl, _ := tablewriter.WrapString(l, 76-w)
				fmt.Printf(wf, "", sl[0])
				for _, l := range sl[1:] {
					fmt.Printf(wf, "", l)
				}
			}
		} else {
			cl := strings.Split(strings.TrimSpace(pi.value), "\n")
			sl, _ := tablewriter.WrapString(cl[0], 76-w)
			fmt.Printf(wf, pi.name, sl[0])
			for _, l := range sl[1:] {
				fmt.Printf(wf, "", l)
			}
			for _, l := range cl[1:] {
				sl, _ := tablewriter.WrapString(l, 76-w)
				for _, l := range sl {
					fmt.Printf(wf, "", l)
				}
			}
		}
	}
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

func lock(lf string) (*os.File, error) {
	// This locking process only works on Windows, on linux os.Remove will remove an open file.
	// This is not currently an issue as running googet on linux is only done for testing.
	// In the future using a semaphore for locking would be nice.
	// 90% of all GooGet runs happen in < 60s, we wait 70s.
	for i := 1; i < 15; i++ {
		// Try to remove any old lock file that may exist, ignore errors as we don't care if
		// we can't remove it or it does not exist.
		os.Remove(lf)
		if lk, err := os.OpenFile(lf, os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0); err == nil {
			return lk, nil
		}
		if i == 1 {
			fmt.Fprintln(os.Stdout, "GooGet lock already held, waiting...")
		}
		time.Sleep(5 * time.Second)
	}
	return nil, errors.New("timed out waiting for lock")
}

func readConf(cf string) {
	gc, err := unmarshalConfFile(cf)
	if err != nil {
		if os.IsNotExist(err) {
			gc = &conf{}
		} else {
			logger.Errorf("Error unmarshalling conf file: %v", err)
		}
	}

	if gc.Archs != nil {
		archs = gc.Archs
	} else {
		archs, err = system.InstallableArchs()
		if err != nil {
			logger.Fatal(err)
		}
	}

	if gc.CacheLife != "" {
		cacheLife, err = time.ParseDuration(gc.CacheLife)
		if err != nil {
			logger.Error(err)
		}
	}

	if gc.ProxyServer != "" {
		proxyServer = gc.ProxyServer
	}

	allowUnsafeURL = gc.AllowUnsafeURL
}

func run() int {
	ggFlags := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	ggFlags.StringVar(&rootDir, "root", os.Getenv(envVar), "googet root directory")
	ggFlags.BoolVar(&noConfirm, "noconfirm", false, "skip confirmation")
	ggFlags.BoolVar(&verbose, "verbose", false, "print info level logs to stdout")
	ggFlags.BoolVar(&systemLog, "system_log", true, "log to Linux Syslog or Windows Event Log")
	ggFlags.BoolVar(&showVer, "version", false, "display GooGet version and exit")

	if err := ggFlags.Parse(os.Args[1:]); err != nil && err != flag.ErrHelp {
		logger.Fatal(err)
	}

	if showVer {
		fmt.Println("GooGet version:", version)
		os.Exit(0)
	}

	cmdr := subcommands.NewCommander(ggFlags, "googet")
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
	cmdr.Register(&listReposCmd{}, "repository management")
	cmdr.Register(&addRepoCmd{}, "repository management")
	cmdr.Register(&rmRepoCmd{}, "repository management")
	cmdr.Register(&cleanCmd{}, "")

	cmdr.ImportantFlag("verbose")
	cmdr.ImportantFlag("noconfirm")

	nonLockingCommands := []string{"help", "commands", "flags"}
	if ggFlags.NArg() == 0 || goolib.ContainsString(ggFlags.Args()[0], nonLockingCommands) {
		return int(cmdr.Execute(context.Background()))
	}

	if rootDir == "" {
		logger.Fatalf("The environment variable %q not defined and no '-root' flag passed.", envVar)
	}
	if err := os.MkdirAll(rootDir, 0774); err != nil {
		logger.Fatalln("Error setting up root directory:", err)
	}

	readConf(filepath.Join(rootDir, confFile))

	lkf := filepath.Join(rootDir, lockFile)
	lk, err := lock(lkf)
	if err != nil {
		logger.Fatal(err)
	}
	defer os.Remove(lkf)
	defer lk.Close()

	logPath := filepath.Join(rootDir, logFile)
	if err := rotateLog(logPath, logSize); err != nil {
		logger.Error(err)
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
	if err != nil {
		logger.Fatalln("Failed to open log file:", err)
	}
	defer lf.Close()

	logger.Init("GooGet", verbose, systemLog, lf)

	if err := os.MkdirAll(filepath.Join(rootDir, cacheDir), 0774); err != nil {
		logger.Fatalf("Error setting up cache directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, repoDir), 0774); err != nil {
		logger.Fatalf("Error setting up repo directory: %v", err)
	}

	return int(cmdr.Execute(context.Background()))
}

func main() {
	os.Exit(run())
}
