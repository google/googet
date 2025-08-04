// Package repo handles repo management tasks.
package repo

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"gopkg.in/yaml.v3"
)

// BuildSources returns a map of repo source to priority value. If an empty
// string is passed in, sources are read from config files in the repo dir.
// Otherwise, sources are pulled from the passed in string.
func BuildSources(s string) (map[string]priority.Value, error) {
	if s == "" {
		return repoList(settings.RepoDir())
	}
	m := make(map[string]priority.Value)
	for _, src := range strings.Split(s, ",") {
		m[src] = priority.Default
	}
	return m, nil
}

// Entry is a single entry in a repo config.
type Entry struct {
	Name     string
	URL      string
	UseOAuth bool
	Priority priority.Value `yaml:",omitempty"`
}

// UnmarshalYAML provides custom unmarshalling for repo Entry objects.
func (r *Entry) UnmarshalYAML(unmarshal func(any) error) error {
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

// File represents a parsed repo configuration file.
type File struct {
	Path    string  // the full path to the file
	Entries []Entry // all of the repo entries in the file
}

// write writes the repo file to disk as YAML if it has any entries.
// If there are no entries, the repo file is removed.
func (rf *File) writeOrDelete() error {
	if len(rf.Entries) == 0 {
		return os.RemoveAll(rf.Path)
	}
	d, err := yaml.Marshal(rf.Entries)
	if err != nil {
		return err
	}
	return os.WriteFile(rf.Path, d, 0664)
}

// addEntry adds a new entry to the repo file if it doesn't already exist,
// otherwise replaces the existing entry and moves it to the end of the file.
// Existing entries are replaced if they have the same name or the same URL.
func (rf *File) addEntry(re Entry) {
	var entries []Entry
	for _, other := range rf.Entries {
		if other.Name != re.Name && other.URL != re.URL {
			entries = append(entries, other)
		}
	}
	entries = append(entries, re)
	rf.Entries = entries
}

// removeEntry removes all entries with given name from the repo file. Returns
// true if any such entry was removed.
func (rf *File) removeEntry(name string) bool {
	var entries []Entry
	var found bool
	for _, re := range rf.Entries {
		if strings.EqualFold(re.Name, name) {
			found = true
			continue
		}
		entries = append(entries, re)
	}
	rf.Entries = entries
	return found
}

func unmarshalRepoFile(p string) (File, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return File{Path: p}, err
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
		return File{Path: p}, nil
	}

	// Both repoFile and []repoFile are valid for backwards compatibility.
	var re Entry
	if err := yaml.Unmarshal(b, &re); err == nil && re.URL != "" {
		return File{Path: p, Entries: []Entry{re}}, nil
	}

	var res []Entry
	if err := yaml.Unmarshal(b, &res); err != nil {
		return File{Path: p}, err
	}
	return File{Path: p, Entries: res}, nil
}

// validateRepoURL determines if u should be checked for https or GCS status
// based on the value of settings.AllowUnsafeURL.
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
	rfs, err := ConfigFiles(dir)
	if err != nil {
		return nil, err
	}
	result := make(map[string]priority.Value)
	for _, rf := range rfs {
		for _, re := range rf.Entries {
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

// ConfigFiles returns the parsed contents of all repo configuration files in
// the given directory.
func ConfigFiles(dir string) ([]File, error) {
	fl, err := filepath.Glob(filepath.Join(dir, "*.repo"))
	if err != nil {
		return nil, err
	}
	var rfs []File
	for _, f := range fl {
		rf, err := unmarshalRepoFile(f)
		if err != nil {
			logger.Error(err)
			continue
		}
		if rf.Path != "" {
			rfs = append(rfs, rf)
		}
	}
	return rfs, nil
}

// AddEntryToFile adds a new repo entry to the given filename. If an entry with
// the same name or url already exists in the repo config file, it is replaced.
func AddEntryToFile(re Entry, filename string) ([]byte, error) {
	content, err := yaml.Marshal([]Entry{re})
	if err != nil {
		return nil, err
	}
	rf, err := unmarshalRepoFile(filename)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	rf.addEntry(re)
	return content, rf.writeOrDelete()
}

// RemoveEntryFromFiles removes any entry with the given name from all repo
// config files in the given directory.
func RemoveEntryFromFiles(name, dir string) ([]string, error) {
	rfs, err := ConfigFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("could not read config files: %v", err)
	}
	var changed []string
	for _, rf := range rfs {
		if rf.removeEntry(name) {
			changed = append(changed, rf.Path)
			if err := rf.writeOrDelete(); err != nil {
				return changed, err
			}
		}
	}
	return changed, nil
}
