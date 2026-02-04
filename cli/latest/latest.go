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

package latest

// The latest subcommand searches the repo for the specified package and returns the latest version.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&latestCmd{}, "package query") }

type latestCmd struct {
	compare bool
	sources string
	json    bool
}

type packageStatus struct {
	PackageName      string `json:"package_name"`
	Status           string `json:"status"`
	InstalledVersion string `json:"installed_version,omitempty"`
	LatestVersion    string `json:"latest_version"`
}

func (*latestCmd) Name() string     { return "latest" }
func (*latestCmd) Synopsis() string { return "print the latest available version of a package" }
func (*latestCmd) Usage() string {
	return fmt.Sprintf("%s latest [-sources repo1,repo2...] [-compare] <name>\n", filepath.Base(os.Args[0]))
}

func (cmd *latestCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.compare, "compare", false, "compare to version locally installed")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
	f.BoolVar(&cmd.json, "json", false, "output status as a JSON object (requires --compare)")

}

func (cmd *latestCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if flags.NArg() != 1 {
		logger.Errorf("Error: exactly one package name argument is required.\nUsage: %s", cmd.Usage())
		return subcommands.ExitUsageError
	}
	if cmd.json && !cmd.compare {
		logger.Error("Error: --json flag requires the --compare flag to be set.")
		return subcommands.ExitUsageError
	}
	pi := goolib.PkgNameSplit(flags.Arg(0))

	repos, err := repo.BuildSources(cmd.sources)
	if err != nil {
		logger.Errorf("Failed to initialize repos: %v", err)
		return subcommands.ExitFailure
	}
	if repos == nil {
		logger.Error("No repos defined, create a .repo file or pass using the -sources flag.")
		return subcommands.ExitFailure
	}

	downloader, err := client.NewDownloader(settings.ProxyServer)
	if err != nil {
		logger.Errorf("Failed to initialize downloader: %v", err)
		return subcommands.ExitFailure
	}

	rm := downloader.AvailableVersions(ctx, repos, settings.CacheDir(), settings.CacheLife)
	spec, _, a, err := client.FindRepoLatest(pi, rm, settings.Archs)
	if err != nil {
		logger.Errorf("Failed to find package: %v", err)
		return subcommands.ExitFailure
	}
	v := spec.Version
	if !cmd.compare {
		fmt.Println(v)
		return subcommands.ExitSuccess
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		logger.Errorf("Failed to open database: %v", err)
		return subcommands.ExitFailure
	}
	defer db.Close()

	state, err := db.FetchPkgs("")
	if err != nil {
		logger.Errorf("Failed fetching installed packages: %v", err)
		return subcommands.ExitFailure
	}
	pi.Arch = a
	var ver string
	pkgFound := false
	for _, p := range state {
		if p.Match(pi) {
			ver = p.PackageSpec.Version
			pkgFound = true
			break
		}
	}

	status := packageStatus{
		PackageName:      pi.Name,
		InstalledVersion: ver,
		LatestVersion:    v,
	}
	if !pkgFound {
		status.Status = "not_installed"
		status.InstalledVersion = ""
	} else {
		c, err := goolib.Compare(v, ver)
		if err != nil {
			logger.Errorf("Failed comparing versions: %v", err)
			return subcommands.ExitFailure
		}
		switch c {
		case 1:
			status.Status = "update_available"
		case 0:
			status.Status = "up_to_date"
		case -1:
			status.Status = "newer_version_installed"
		}
	}
	if cmd.json {
		out, err := json.Marshal(status)
		if err != nil {
			logger.Errorf("Failed to marshal JSON: %v", err)
			return subcommands.ExitFailure
		}
		fmt.Println(string(out))
		return subcommands.ExitSuccess
	}

	// If not JSON, use  human readable format.
	switch status.Status {
	case "update_available":
		fmt.Printf("%s: Update available (current: %s, latest: %s)\n", status.PackageName, status.InstalledVersion, status.LatestVersion)
	case "up_to_date":
		fmt.Printf("%s: Up-to-date (%s)\n", status.PackageName, status.InstalledVersion)
	case "newer_version_installed":
		fmt.Printf("%s: Newer version installed (current: %s, repo: %s)\n", status.PackageName, status.InstalledVersion, status.LatestVersion)
	case "not_installed":
		fmt.Printf("%s: Not installed (latest available: %s)\n", status.PackageName, status.LatestVersion)
	}
	return subcommands.ExitSuccess
}
