// Package settings stores various googet settings.
package settings

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/google/googet/v2/system"
	"github.com/google/logger"
	"gopkg.in/yaml.v3"
)

var (
	// RootDir is the googet root directory; set by Initialize.
	RootDir string
	// Confirm tracks whether we should prompt user; set by Initialize.
	Confirm bool
	// CacheLife is how long cached indexes are considered valid.
	// The default value can be overridden by googet.conf.
	CacheLife = 3 * time.Minute
	// LockFileMaxAge is the maximum age of a lock file before it's considered stale.
	LockFileMaxAge = 24 * time.Hour
	// Archs is the list of valid arches for the system.
	// Taken from googet.conf, or else derived from runtime.GOARCH.
	Archs []string
	// ProxyServer is used by the HTTP client; set from googet.conf.
	ProxyServer string
	// AllowUnsafeURL allows HTTP repos; set from googet.conf.
	AllowUnsafeURL bool
)

// Initialize reads the initial settings.
func Initialize(rootDir string, confirm bool) {
	RootDir = rootDir
	Confirm = confirm
	readConf(ConfFile())
}

// LockFile returns the path to the googet lock file.
func LockFile() string {
	return filepath.Join(RootDir, "googet.lock")
}

// StateFile returns the path to the JSON package state.
// DEPRECATED: The state file was replaced by the googet database.
func StateFile() string {
	return filepath.Join(RootDir, "googet.state")
}

// DBFile returns the path to the installed package state database.
func DBFile() string {
	return filepath.Join(RootDir, "googet.db")
}

// ConfFile returns the path to the googet configuration file.
func ConfFile() string {
	return filepath.Join(RootDir, "googet.conf")
}

// LogFile returns the path to the googet log.
func LogFile() string {
	return filepath.Join(RootDir, "googet.log")
}

// CacheDir returns the path to the index / package cache.
func CacheDir() string {
	return filepath.Join(RootDir, "cache")
}

// RepoDir returns the path to the repo config files.
func RepoDir() string {
	return filepath.Join(RootDir, "repos")
}

// conf represents a googet configuration file.
type conf struct {
	Archs          []string
	CacheLife      string
	LockFileMaxAge string
	ProxyServer    string
	AllowUnsafeURL bool
}

// unmarshalConfFile returns a conf from a YAML configuration file.
func unmarshalConfFile(filename string) (*conf, error) {
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var cf conf
	return &cf, yaml.Unmarshal(b, &cf)
}

// readConf initializes settings based on the configuration file at cf.
func readConf(filename string) {
	gc, err := unmarshalConfFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			gc = &conf{}
		} else {
			// TODO: if ReadFile fails for some reason other than the file not existing,
			// gc is nil and the code below will panic after falling through here.
			logger.Errorf("Error unmarshalling conf file: %v", err)
		}
	}

	if gc.Archs != nil {
		Archs = gc.Archs
	} else {
		Archs, err = system.InstallableArchs()
		if err != nil {
			logger.Fatal(err)
		}
	}

	if gc.CacheLife != "" {
		CacheLife, err = time.ParseDuration(gc.CacheLife)
		if err != nil {
			logger.Error(err)
		}
	}

	if gc.LockFileMaxAge != "" {
		LockFileMaxAge, err = time.ParseDuration(gc.LockFileMaxAge)
		if err != nil {
			logger.Error(err)
		}
	}

	if gc.ProxyServer != "" {
		ProxyServer = gc.ProxyServer
	}

	AllowUnsafeURL = gc.AllowUnsafeURL
}
