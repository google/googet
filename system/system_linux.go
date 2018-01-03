// +build linux

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
	"path/filepath"

	"github.com/google/googet/goolib"
	"github.com/google/googet/oswrap"
	"github.com/google/logger"
)

// Install performs a system specfic install given a package extraction directory and an PkgSpec struct.
func Install(dir string, ps *goolib.PkgSpec) error {
	in := ps.Install
	if in.Path == "" {
		logger.Info("No installer specified")
		return nil
	}

	logger.Infof("Running install: %q", in.Path)
	out, err := oswrap.Create(filepath.Join(dir, "googet_install.log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	if err := goolib.Exec(filepath.Join(dir, in.Path), in.Args, in.ExitCodes, out); err != nil {
		return fmt.Errorf("error running install: %v", err)
	}
	return nil
}

// Uninstall performs a system specfic uninstall given a packages PackageState.
func Uninstall(dir string, ps *goolib.PkgSpec) error {
	un := ps.Uninstall
	if un.Path == "" {
		return nil
	}

	logger.Infof("Running uninstall: %q", un.Path)
	// logging is only useful for failed uninstalls
	out, err := oswrap.Create(filepath.Join(dir, "googet_remove.log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	return goolib.Exec(filepath.Join(dir, un.Path), un.Args, un.ExitCodes, out)
}

// InstallableArchs returns a slice of archs supported by this machine.
func InstallableArchs() ([]string, error) {
	// Just return all archs as Linux builds are currently just used for testing.
	return []string{"noarch", "x86_64", "x86_32", "arm"}, nil
}
