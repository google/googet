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

// Package system handles system specific functions.
package system

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/StackExchange/wmi"
	"github.com/google/googet/client"
	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
	"golang.org/x/sys/windows/registry"
)

const regBase = "SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\"

var msiSuccessCodes = []int{1641, 3010}

func addUninstallEntry(dir string, ps *goolib.PkgSpec) error {
	reg := regBase + "GooGet - " + ps.Name
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
		{"UninstallString", fmt.Sprintf("%s -no_confirm remove %s", exe, ps.Name)},
		{"InstallLocation", dir},
		{"DisplayVersion", ps.Version},
		{"DisplayName", "GooGet - " + ps.Name},
	}
	for _, re := range table {
		if err := k.SetStringValue(re.name, re.value); err != nil {
			return err
		}
	}
	return nil
}

func removeUninstallEntry(name string) error {
	reg := regBase + "GooGet - " + name
	logger.Infof("Removing uninstall entry %q from registry.", reg)
	return registry.DeleteKey(registry.LOCAL_MACHINE, reg)
}

// Install performs a system specfic install given a package extraction directory and a PkgSpec struct.
func Install(dir string, ps *goolib.PkgSpec) error {
	in := ps.Install
	if in.Path == "" {
		logger.Info("No installer specified")
		return nil
	}

	logger.Infof("Running install: %q", in.Path)
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
	switch filepath.Ext(s) {
	case ".msi":
		args := append([]string{"/i", s, "/qn", "/norestart", "/log", msiLog}, in.Args...)
		ec := append(msiSuccessCodes, in.ExitCodes...)
		err = goolib.Run(exec.Command("msiexec", args...), ec, out)
	case ".msp":
		args := append([]string{"/update", s, "/qn", "/norestart", "/log", msiLog}, in.Args...)
		ec := append(msiSuccessCodes, in.ExitCodes...)
		err = goolib.Run(exec.Command("msiexec", args...), ec, out)
	case ".msu":
		args := append([]string{s, "/quiet", "/norestart"}, in.Args...)
		err = goolib.Run(exec.Command("wusa", args...), in.ExitCodes, out)
	case ".exe":
		err = goolib.Run(exec.Command(s, in.Args...), in.ExitCodes, out)
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
func Uninstall(st client.PackageState) error {
	un := st.PackageSpec.Uninstall
	if un.Path == "" {
		logger.Info("No uninstaller specified")
		return nil
	}

	logger.Infof("Running uninstall: %q", un.Path)
	// logging is only useful for failed uninstall
	out, err := oswrap.Create(filepath.Join(st.UnpackDir, un.Path+".log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	s := filepath.Join(st.UnpackDir, un.Path)
	switch filepath.Ext(s) {
	case ".msi":
		msiLog := filepath.Join(st.UnpackDir, "msi_uninstall.log")
		args := append([]string{"/x", s, "/qn", "/norestart", "/log", msiLog}, un.Args...)
		ec := append(msiSuccessCodes, un.ExitCodes...)
		err = goolib.Run(exec.Command("msiexec", args...), ec, out)
	case ".msu":
		args := append([]string{s, "/uninstall", "/quiet", "/norestart"}, un.Args...)
		err = goolib.Run(exec.Command("wusa", args...), un.ExitCodes, out)
	case ".exe":
		err = goolib.Run(exec.Command(s, un.Args...), un.ExitCodes, out)
	default:
		err = goolib.Exec(filepath.Join(st.UnpackDir, un.Path), un.Args, un.ExitCodes, out)
	}
	if err != nil {
		return err
	}

	if err := removeUninstallEntry(st.PackageSpec.Name); err != nil {
		logger.Error(err)
	}
	return nil
}

type win32_OperatingSystem struct {
	AddressWidth uint16
}

func width() (int, error) {
	var os []win32_OperatingSystem
	if err := wmi.Query(wmi.CreateQuery(&os, ""), &os); err != nil {
		return 0, err
	}
	return int(os[0].AddressWidth), nil
}

// InstallableArchs returns a slice of archs supported by this machine.
// WMI errors are logged but not returned.
func InstallableArchs() ([]string, error) {
	switch {
	case runtime.GOARCH == "386":
		// Check if this is indeed a 32bit system.
		aw, err := width()
		if err != nil {
			logger.Errorf("Error getting AddressWidth: %v", err)
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
