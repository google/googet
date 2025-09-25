//go:build windows
// +build windows

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

package system

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
	"github.com/yusufpapurcu/wmi"
	"golang.org/x/sys/windows/registry"
)

const uninstallBase = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\`

// Regex taken from Winget uninstaller
// https://github.com/microsoft/winget-cli/blob/6ea13623e5e4b870b81efeea9142d15a98dd4208/src/AppInstallerCommonCore/NameNormalization.cpp#L262
var (
	programNameReg = []string{
		"PrefixParens",
		"EmptyParens",
		"Version",
		"TrailingSymbols",
		"LeadingSymbols",
		"FilePathParens",
		"FilePathQuotes",
		"FilePath",
		"VersionLetter",
		"VersionLetterDelimited",
		"En",
		"NonNestedBracket",
		"BracketEnclosed",
		"URIProtocol",
	}
	publisherNameReg = []string{
		"VersionDelimited",
		"Version",
		"NonNestedBracket",
		"BracketEnclosed",
		"URIProtocol",
	}
	regex = map[string]string{
		"PrefixParens":           `(^\(.*?\))`,
		"EmptyParens":            `((\(\s*\)|\[\s*\]|"\s*"))`,
		"Version":                `(?:^)|(?:P|V|R|VER|VERSI(?:O|Ó)N|VERSÃO|VERSIE|WERSJA|BUILD|RELEASE|RC|SP)(?:\P{L}|\P{L}\p{L})?(\p{Nd}|\.\p{Nd})+(?:RC|B|A|R|V|SP)?\p{Nd}?`,
		"TrailingSymbols":        `([^\p{L}\p{Nd}]+$)`,
		"LeadingSymbols":         `(^[^\p{L}\p{Nd}]+)`,
		"FilePathParens":         `(\([CDEF]:\\(.+?\\)*[^\s]*\\?\))`,
		"FilePathQuotes":         `("[CDEF]:\\(.+?\\)*[^\s]*\\?")`,
		"FilePath":               `(((INSTALLED\sAT|IN)\s)?[CDEF]:\\(.+?\\)*[^\s]*\\?)`,
		"VersionLetter":          `((?:^\p{L})(?:(?:V|VER|VERSI(?:O|Ó)N|VERSÃO|VERSIE|WERSJA|BUILD|RELEASE|RC|SP)\P{L})?\p{Lu}\p{Nd}+(?:[\p{Po}\p{Pd}\p{Pc}]\p{Nd}+)+)`,
		"VersionLetterDelimited": `((?:^\p{L})(?:(?:V|VER|VERSI(?:O|Ó)N|VERSÃO|VERSIE|WERSJA|BUILD|RELEASE|RC|SP)\P{L})?\p{Lu}\p{Nd}+(?:[\p{Po}\p{Pd}\p{Pc}]\p{Nd}+)+)`,
		"En":                     `(\sEN\s*$)`,
		"NonNestedBracket":       `(\([^\(\)]*\)|\[[^\[\]]*\])`,
		"BracketEnclosed":        `((?:\p{Ps}.*\p{Pe}|".*"))`,
		"URIProtocol":            `((?:^\p{L})(?:http[s]?|ftp):\/\/)`,
	}
	msiSuccessCodes = []int{1641, 3010}
)

func addUninstallEntry(dir string, ps *goolib.PkgSpec) error {
	reg := uninstallBase + "GooGet - " + ps.Name
	logger.Infof("Adding uninstall entry %q to registry.", reg)
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, reg, registry.WRITE)
	if err != nil {
		return err
	}
	defer k.Close()

	exe := filepath.Join(os.Getenv("GooGetRoot"), "googet.exe")

	table := []struct {
		name, value string
	}{
		{"UninstallString", fmt.Sprintf("%s -noconfirm remove %s", exe, ps.Name)},
		{"InstallLocation", dir},
		{"DisplayVersion", ps.Version},
		{"DisplayName", "GooGet - " + ps.Name},
		{"InstallDate", time.Now().Format("20060102")},
	}
	for _, re := range table {
		if err := k.SetStringValue(re.name, re.value); err != nil {
			return err
		}
	}
	return nil
}

func removeUninstallEntry(name string) error {
	reg := uninstallBase + "GooGet - " + name
	logger.Infof("Removing uninstall entry %q from registry.", reg)
	return registry.DeleteKey(registry.LOCAL_MACHINE, reg)
}

// AppAssociation locates and returns registry entry and name of installed application.
func AppAssociation(ps *goolib.PkgSpec, installSource string) (string, string) {
	// Packages with files are portable and don't have actual installers.
	if ps.Files != nil {
		return "", ""
	}
	var productroots = []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\`,
		`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\`,
	}
	for _, productroot := range productroots {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, productroot, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			return "", ""
		}
		defer k.Close()
		reg, err := k.ReadSubKeyNames(-1)
		if err != nil {
			return "", ""
		}
		for _, v := range reg {
			productReg := fmt.Sprintf("%s%s", productroot, v)
			q, err := registry.OpenKey(registry.LOCAL_MACHINE, productReg, registry.ALL_ACCESS)
			if err != nil {
				continue
			}
			defer q.Close()
			displayName, _, err := q.GetStringValue("DisplayName")
			if err != nil {
				continue
			}

			if filepath.Ext(ps.Install.Path) == ".msi" && installSource != "" {
				a, _, err := q.GetStringValue("InstallSource")
				if err != nil {
					// InstallSource not found, move on to next entry
					continue
				}
				iS := strings.Split(installSource, "@")
				if strings.Contains(a, iS[0]) {
					name, _, err := q.GetStringValue("DisplayName")
					if err != nil {
						// UninstallString not found, move on to next entry
						continue
					}
					return name, productReg
				}
			}
			if displayName == ps.ExternalProgramName && ps.ExternalProgramName != "" {
				return displayName, productReg
			}
			// TODO: Look into precompiling regex
			var publisher, programName string
			for _, v := range publisherNameReg {
				re := regexp.MustCompile("(?i)" + regex[v])
				publisher = re.ReplaceAllString(ps.Authors, "")
			}
			for _, v := range programNameReg {
				re := regexp.MustCompile("(?i)" + regex[v])
				programName = re.ReplaceAllString(ps.Name, "")
			}
			// Ignore empty and googet labeled pacakges
			if displayName == "" || strings.Contains(displayName, "GooGet -") {
				continue
			}
			// Check if Package name is in display name removing spaces
			regPublisher, _, err := q.GetStringValue("Publisher")
			if err != nil {
				continue
			}
			// Be more restrictive on publisher match for all software, to reduce the possibility of false positives.
			if !strings.EqualFold(regPublisher, publisher) {
				continue
			}
			if strings.Contains(strings.ToLower(strings.ReplaceAll(displayName, " ", "")), strings.ToLower(programName)) {
				return displayName, productReg
			}
			// Check if Package name is in display name removing dashes
			if strings.Contains(strings.ToLower(strings.ReplaceAll(displayName, "-", "")), strings.ToLower(programName)) {
				return displayName, productReg
			}
			a, _, err := q.GetStringValue("InstallSource")
			if err != nil {
				// InstallSource not found, move on to next entry
				continue
			}
			if installSource == "" {
				return "", ""
			}
			iS := strings.Split(installSource, "@")
			if strings.Contains(a, iS[0]) && installSource != "" {
				return displayName, productReg
			}
		}
	}
	return "", ""
}

func uninstallString(regkey, extension string) string {
	q, err := registry.OpenKey(registry.LOCAL_MACHINE, regkey, registry.ALL_ACCESS)
	if err != nil {
		logger.Error(err)
		return ""
	}
	defer q.Close()
	switch extension {
	case "msi":
		un, _, err := q.GetStringValue("UninstallString")
		if err != nil {
			logger.Error(err)
			// UninstallString not found, move on
			return ""
		}
		return un
	default:
		un, _, err := q.GetStringValue("QuietUninstallString")
		if err != nil {
			un, _, err = q.GetStringValue("UninstallString")
			if err != nil {
				// UninstallString not found, move on to next entry
				return ""
			}
		}
		return un
	}

}

// Install performs a system specfic install given a package extraction directory and a PkgSpec struct.
func Install(dir string, ps *goolib.PkgSpec) error {
	in := ps.Install
	if in.Path == "" {
		return nil
	}

	logger.Infof("Running install command: %q", in.Path)
	out, err := oswrap.Create(filepath.Join(dir, in.Path+".log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	s := filepath.Join(dir, in.Path)
	msiLog := filepath.Join(dir, "msi_install.log")
	ec := append(msiSuccessCodes, in.ExitCodes...)
	switch filepath.Ext(s) {
	case ".msi":
		args := append([]string{"/i", s, "/qn", "/norestart", "/log", msiLog}, in.Args...)
		err = goolib.Run(exec.Command("msiexec", args...), ec, out)
	case ".msp":
		args := append([]string{"/update", s, "/qn", "/norestart", "/log", msiLog}, in.Args...)
		err = goolib.Run(exec.Command("msiexec", args...), ec, out)
	case ".msu":
		args := append([]string{s, "/quiet", "/norestart"}, in.Args...)
		err = goolib.Run(exec.Command("wusa", args...), ec, out)
	case ".exe":
		err = goolib.Run(exec.Command(s, in.Args...), ec, out)
	case ".msix", ".msixbundle":
		// Add-AppxProvisionedPackage will install for all users.
		installCmd := fmt.Sprintf("Add-AppxProvisionedPackage -online -PackagePath %v -SkipLicense", s)
		args := append([]string{installCmd}, in.Args...)
		err = goolib.Run(exec.Command("powershell", args...), ec, out)
	default:
		err = goolib.Exec(s, in.Args, in.ExitCodes, out)
	}
	if err != nil {
		return err
	}

	if err := addUninstallEntry(dir, ps); err != nil {
		logger.Error(err)
	}

	return nil
}

// Uninstall performs a system specfic uninstall given a packages PackageState.
func Uninstall(dir string, state *client.PackageState) error {
	var filePath string
	ps := state.PackageSpec
	un := ps.Uninstall
	r := regexp.MustCompile(`[^\s"]+|"([^"]*)"`)
	// Automatically determine uninstall script if none is specified in spec.
	if un.Path == "" {
		switch filepath.Ext(ps.Install.Path) {
		case ".msi":
			u := uninstallString(state.InstalledApp.Reg, "msi")
			if u == "" {
				return nil
			}
			u = strings.ReplaceAll(u, `/I`, `/X`)
			commands := r.FindAllString(u, -1)
			un.Path = strings.Replace(commands[0], "\"", "", -1)
			un.Args = []string{"/qn", "/norestart"}
			un.Args = append(commands[1:], un.Args...)
			filePath = un.Path
		case ".msix", ".msixbundle":
			un.Path = ps.Install.Path
			filePath = un.Path
		default:
			u := uninstallString(state.InstalledApp.Reg, "")
			commands := r.FindAllString(u, -1)
			if len(commands) > 0 {
				// Remove the quotes from the install string since we handle that below
				un.Path = strings.Replace(commands[0], "\"", "", -1)
				un.Args = commands[1:]
				filePath = un.Path
			}
		}
		if un.Path == "" {
			return nil
		}
	}
	logger.Infof("Running uninstall command: %q", un.Path)
	// logging is only useful for failed uninstall
	// Only append the directory if the folder structure doesn't exist
	logPath := fmt.Sprintf("%s.log", un.Path)
	if _, err := os.Stat(un.Path); errors.Is(err, os.ErrNotExist) {
		logPath = filepath.Join(dir, logPath)
	}
	out, err := oswrap.Create(logPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	if filePath == "" {
		filePath = filepath.Join(dir, un.Path)
	}
	ec := append(msiSuccessCodes, un.ExitCodes...)
	switch filepath.Ext(filePath) {
	case ".msi":
		msiLog := filepath.Join(dir, "msi_uninstall.log")
		args := append([]string{"/x", filePath, "/qn", "/norestart", "/log", msiLog}, un.Args...)
		err = goolib.Run(exec.Command("msiexec", args...), ec, out)
	case ".msu":
		args := append([]string{filePath, "/uninstall", "/quiet", "/norestart"}, un.Args...)
		err = goolib.Run(exec.Command("wusa", args...), ec, out)
	case ".exe":
		err = goolib.Run(exec.Command(filePath, un.Args...), ec, out)
	case ".msix", ".msixbundle":
		s := strings.Split(filepath.Base(filePath), "_")[0]
		removeCmd := fmt.Sprintf(`Get-AppxProvisionedPackage -online | Where {$_.DisplayName -match "%v*"} | Remove-AppProvisionedPackage -online -AllUsers`, s)
		args := append([]string{removeCmd}, un.Args...)
		err = goolib.Run(exec.Command("powershell", args...), ec, out)
	default:
		err = goolib.Exec(filepath.Join(dir, un.Path), un.Args, un.ExitCodes, out)
	}
	if err != nil {
		return err
	}

	if err := removeUninstallEntry(ps.Name); err != nil {
		logger.Error(err)
	}

	return nil
}

type Win32_Processor struct {
	AddressWidth uint16
}

func width() (int, error) {
	var p []Win32_Processor
	if err := wmi.Query(wmi.CreateQuery(&p, ""), &p); err != nil {
		return 0, err
	}
	return int(p[0].AddressWidth), nil
}

// InstallableArchs returns a slice of archs supported by this machine.
// WMI errors are logged but not returned.
func InstallableArchs() ([]string, error) {
	switch {
	case runtime.GOARCH == "386":
		// Check if this is indeed a 32bit system.
		aw, err := width()
		if err != nil {
			return []string{"noarch", "x86_32"}, nil
		}
		if int(aw) == 32 {
			return []string{"noarch", "x86_32"}, nil
		}
		return []string{"noarch", "x86_64", "x86_32"}, nil
	case runtime.GOARCH == "amd64":
		// TODO: Add check for 32bit support, make sure it works with servers and client OS's.
		return []string{"noarch", "x86_32", "x86_64"}, nil
	case runtime.GOARCH == "arm":
		return []string{"noarch", "arm"}, nil
	case runtime.GOARCH == "arm64":
		return []string{"noarch", "x86_32", "x86_64", "arm64"}, nil
	default:
		return nil, fmt.Errorf("runtime %s not supported", runtime.GOARCH)
	}
}

// IsAdmin checks to see if a user is admin and in an elevated prompt.
func IsAdmin() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("must be run in an elevated (administrative) session")
	}
	return nil
}

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	// https://docs.microsoft.com/en-us/windows/desktop/api/fileapi/nf-fileapi-lockfileex
	LOCKFILE_EXCLUSIVE_LOCK   = 2
	LOCKFILE_FAIL_IMMEDIATELY = 1
)

func lockFileEx(hFile uintptr, dwFlags, nNumberOfBytesToLockLow, nNumberOfBytesToLockHigh uint32, lpOverlapped *syscall.Overlapped) (err error) {
	ret, _, _ := procLockFileEx.Call(
		hFile,
		uintptr(dwFlags),
		0,
		uintptr(nNumberOfBytesToLockLow),
		uintptr(nNumberOfBytesToLockHigh),
		uintptr(unsafe.Pointer(lpOverlapped)),
	)
	// If the function succeeds, the return value is nonzero.
	if ret == 0 {
		return errors.New("LockFileEx unable to obtain lock")
	}
	return nil
}

func unlockFileEx(hFile uintptr, nNumberOfBytesToLockLow, nNumberOfBytesToLockHigh uint32, lpOverlapped *syscall.Overlapped) (err error) {
	ret, _, _ := procUnlockFileEx.Call(
		hFile,
		0,
		uintptr(nNumberOfBytesToLockLow),
		uintptr(nNumberOfBytesToLockHigh),
		uintptr(unsafe.Pointer(lpOverlapped)),
	)
	// If the function succeeds, the return value is nonzero.
	if ret == 0 {
		return errors.New("UnlockFileEx unable to unlock")
	}
	return nil
}

// lock obtains a lock on a file.
func lock(f *os.File) (func(), error) {
	if err := lockFileEx(f.Fd(), LOCKFILE_EXCLUSIVE_LOCK, 1, 0, &syscall.Overlapped{}); err != nil {
		return nil, err
	}
	return func() {
		unlockFileEx(f.Fd(), 1, 0, &syscall.Overlapped{})
		f.Close()
		os.Remove(f.Name())
	}, nil
}
