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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
	"github.com/yusufpapurcu/wmi"
	"golang.org/x/sys/windows/registry"
)

const uninstallBase = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\`

var msiSuccessCodes = []int{1641, 3010}

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

func uninstallString(installSource, extension string) string {
	productroot := `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, productroot, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ""
	}
	reg, _ := k.ReadSubKeyNames(-1)
	defer k.Close()
	for _, v := range reg {
		q, err := registry.OpenKey(registry.LOCAL_MACHINE, fmt.Sprintf("%s%s", productroot, v), registry.ALL_ACCESS)
		defer q.Close()
		if err != nil {
			continue
		}
		switch extension {
		case "msi":
			a, _, err := q.GetStringValue("InstallSource")
			if err != nil {
				// InstallSource not found, move on to next entry
				continue
			}
			if strings.Contains(a, installSource) {
				un, _, err := q.GetStringValue("UninstallString")
				if err != nil {
					// UninstallString not found, move on to next entry
					continue
				}
				return un
			}
		}
	}
	return ""
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
func Uninstall(dir string, ps *goolib.PkgSpec) error {
	var filePath string
	un := ps.Uninstall
	// Automatically determine uninstall script if none is specified in spec.
	if un.Path == "" {
		switch filepath.Ext(ps.Install.Path) {
		case ".msi":
			u := uninstallString(dir, "msi")
			u = strings.ReplaceAll(u, `/I`, `/X`)
			commands := strings.Split(u, " ")
			un.Path = commands[0]
			un.Args = commands[1:]
			un.Args = append([]string{"/qn", "/norestart"}, un.Args...)
			filePath = un.Path
		}
		if un.Path == "" {
			return nil
		}
	}

	logger.Infof("Running uninstall command: %q", un.Path)
	// logging is only useful for failed uninstall
	out, err := oswrap.Create(filepath.Join(dir, un.Path+".log"))
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
	default:
		return nil, fmt.Errorf("runtime %s not supported", runtime.GOARCH)
	}
}
