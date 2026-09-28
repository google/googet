// Package settings stores various googet settings.
package settings

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/google/googet/v2/supervisor"
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
	// StrictConflicts enables strict enforcement of file ownership conflicts.
	StrictConflicts bool
	// SupervisorMode is the installer watchdog mode parsed from googet.conf ("enforce",
	// "monitor" or "off"). ModeUnset means the built-in default.
	SupervisorMode supervisor.Mode
	// InactivityTimeout is how long an installer may make no forward progress before it is terminated.
	// Zero means the built-in default and a negative value disables the watchdog.
	InactivityTimeout time.Duration
	// InstallTimeout is the absolute runtime limit for an installer.
	// Zero means the built-in default and a negative value disables the limit.
	InstallTimeout time.Duration
	// UIGracePeriod is how long an interactive dialog may persist without progress before termination.
	// Zero means the built-in default.
	UIGracePeriod time.Duration
	// UIDetection enables interactive dialog detection in unattended mode.
	UIDetection = true
	// DownloadStallTimeout is how long a download may receive zero bytes before it is retried.
	// Zero means the built-in default.
	DownloadStallTimeout time.Duration
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
	Archs                []string
	CacheLife            string
	LockFileMaxAge       string
	ProxyServer          string
	AllowUnsafeURL       bool
	StrictConflicts      bool
	SupervisorMode       string
	InactivityTimeout    string
	InstallTimeout       string
	UIGracePeriod        string
	UIDetection          *bool
	DownloadStallTimeout string
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
		cl, err := time.ParseDuration(gc.CacheLife)
		if err != nil {
			logger.Error(err)
		} else {
			CacheLife = cl
		}
	}

	if gc.LockFileMaxAge != "" {
		lfma, err := time.ParseDuration(gc.LockFileMaxAge)
		if err != nil {
			logger.Error(err)
		} else {
			LockFileMaxAge = lfma
		}
	}

	if gc.ProxyServer != "" {
		ProxyServer = gc.ProxyServer
	}

	AllowUnsafeURL = gc.AllowUnsafeURL
	StrictConflicts = gc.StrictConflicts

	SupervisorMode = parseMode(gc.SupervisorMode)
	InactivityTimeout = parseTimeout("InactivityTimeout", gc.InactivityTimeout, true)
	InstallTimeout = parseTimeout("InstallTimeout", gc.InstallTimeout, true)
	UIGracePeriod = parseTimeout("UIGracePeriod", gc.UIGracePeriod, false)
	DownloadStallTimeout = parseTimeout("DownloadStallTimeout", gc.DownloadStallTimeout, false)
	UIDetection = gc.UIDetection == nil || *gc.UIDetection
}

// parseMode parses the googet.conf SupervisorMode. Empty or invalid values return
// supervisor.ModeUnset (use the built-in default); invalid values are logged.
func parseMode(s string) supervisor.Mode {
	if s == "" {
		return supervisor.ModeUnset
	}
	m, err := supervisor.ParseMode(s)
	if err != nil {
		logger.Errorf("Invalid SupervisorMode in googet.conf, using default: %v", err)
		return supervisor.ModeUnset
	}
	return m
}

// parseTimeout parses a googet.conf duration with supervisor.ParseTimeout. Empty or
// invalid values return zero (use the built-in default) and invalid values are logged.
// If allowDisable is set, "0" returns a negative value meaning disabled; otherwise "0" is
// rejected as invalid.
func parseTimeout(name, s string, allowDisable bool) time.Duration {
	d, err := supervisor.ParseTimeout(s)
	switch {
	case err != nil:
		logger.Errorf("Invalid %s in googet.conf, using default: %v", name, err)
		return 0
	case d < 0 && !allowDisable:
		logger.Errorf("Invalid %s %q in googet.conf, using default: must be positive", name, s)
		return 0
	}
	return d
}
