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

package addrepo

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&addRepoCmd{}, "repository management") }

type addRepoCmd struct {
	file     string
	priority string
	dryRun   bool
}

func (*addRepoCmd) Name() string     { return "addrepo" }
func (*addRepoCmd) Synopsis() string { return "add repository" }
func (*addRepoCmd) Usage() string {
	return fmt.Sprintf(`%s addrepo [-file <repofile>] [-priority <value>] [-dry_run] <name> <url>:
	Add repository to GooGet's repository list.
	If -file is not set 'name.repo' will be used for the file name
	overwriting any existing file with that name.
	If -file is set the specified repo will be appended to that repo file,
	creating it if it does not exist.
	If -priority is specified, the repo will be configured with this priority level.
	The -dry_run flag will simulate adding the repo without making changes.
`, filepath.Base(os.Args[0]))
}

func (cmd *addRepoCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&cmd.file, "file", "", "repo file to add this repository to")
	f.StringVar(&cmd.priority, "priority", "", "priority level assigned to repository")
	f.BoolVar(&cmd.dryRun, "dry_run", false, "simulate adding the repo without making changes")
}

func (cmd *addRepoCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if got, want := f.NArg(), 2; got != want {
		fmt.Fprintf(os.Stderr, "Wrong number of arguments: got %v, want %v\n", got, want)
		f.Usage()
		return subcommands.ExitUsageError
	}

	entry := repo.Entry{Name: f.Arg(0), URL: f.Arg(1)}

	fileName, err := repoFileName(cmd.file, entry.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid repo file name: %v\n", err)
		return subcommands.ExitUsageError
	}

	if cmd.priority != "" {
		entry.Priority, err = priority.FromString(cmd.priority)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unrecognized priority value: %q\n", cmd.priority)
			return subcommands.ExitUsageError
		}
	}

	repoFile := filepath.Join(settings.RepoDir(), fileName)
	// Refuse to follow a symlink, junction or other non-regular file, which
	// could make the write land outside the repos directory.
	if fi, err := os.Lstat(repoFile); err == nil && !fi.Mode().IsRegular() {
		fmt.Fprintf(os.Stderr, "Invalid repo file name: %q is not a regular file in the repos directory\n", fileName)
		return subcommands.ExitUsageError
	} else if err != nil && !os.IsNotExist(err) {
		logger.Error(err)
		return subcommands.ExitFailure
	}

	if cmd.dryRun {
		fmt.Printf("Dry run: Would add repo %q with URL %q to %q.\n", entry.Name, entry.URL, repoFile)
		if cmd.priority != "" {
			fmt.Printf("Dry run: Repository priority would be set to %q.\n", cmd.priority)
		}
		return subcommands.ExitSuccess
	}

	content, err := repo.AddEntryToFile(entry, repoFile)
	if err != nil {
		logger.Error(err)
		return subcommands.ExitFailure
	}

	fmt.Printf("Appended to repo file %s with the following content:\n%s\n", repoFile, content)
	return subcommands.ExitSuccess
}

// repoFileName returns the name of the repo file to add the entry to: file if
// set, otherwise name with a '.repo' suffix. It returns an error unless the
// result is a plain file name that stays inside the repos directory.
func repoFileName(file, name string) (string, error) {
	derived := file == ""
	if derived {
		file = name + ".repo"
	} else if !strings.HasSuffix(file, ".repo") {
		return "", fmt.Errorf("%q must end in '.repo'", file)
	}
	if err := checkRepoFileName(file); err != nil {
		if derived {
			return "", fmt.Errorf("%v (derived from repo name %q, use -file to choose a file name)", err, name)
		}
		return "", err
	}
	return file, nil
}

// checkRepoFileName returns an error unless file is a plain file name with a
// non-empty name before the '.repo' suffix. Path separators of either kind and
// ':' are rejected on every platform, which also rules out '..' components,
// absolute paths, drive letters and UNC paths. Windows reserved device names
// are also rejected on every platform, so the same names are valid everywhere.
func checkRepoFileName(file string) error {
	if strings.ContainsAny(file, `/\:`) {
		return fmt.Errorf(`%q must be a plain file name in the repos directory, without '/', '\' or ':'`, file)
	}
	if strings.TrimSuffix(file, ".repo") == "" {
		return fmt.Errorf("%q must have a name before '.repo'", file)
	}
	if isReservedWindowsName(file) {
		return fmt.Errorf("%q is a reserved device name on Windows", file)
	}
	// This is a deliberate backstop in case the checks above are loosened.
	if !filepath.IsLocal(file) {
		return fmt.Errorf("%q must be a plain file name in the repos directory", file)
	}
	return nil
}

// isReservedWindowsName reports whether Windows may map the file name to a
// device: CON, PRN, AUX, NUL, CONIN$, CONOUT$, or COM or LPT followed by a
// digit 0-9 or one of the superscript digits ¹, ² and ³, such as COM1 or
// LPT². Windows ignores everything from the first '.' and any trailing spaces
// when it matches a device name, and compares device names without regard to
// ASCII case.
func isReservedWindowsName(file string) bool {
	base, _, _ := strings.Cut(file, ".")
	base = strings.TrimRight(base, " ")
	switch asciiUpper(base) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) < 4 {
		return false
	}
	switch asciiUpper(base[:3]) {
	case "COM", "LPT":
		switch base[3:] {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3":
			return true
		}
	}
	return false
}

// asciiUpper returns s with ASCII lowercase letters mapped to uppercase and all
// other bytes unchanged. Unlike strings.ToUpper, it does not map non-ASCII
// letters such as dotless 'ı' to ASCII ones.
func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}
