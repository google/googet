/*
Copyright 2018 Google Inc. All Rights Reserved.
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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

// Verify runs a verify command given a package extraction directory and a PkgSpec struct.
func Verify(dir string, ps *goolib.PkgSpec) error {
	v := ps.Verify
	if v.Path == "" {
		return nil
	}

	logger.Infof("Running verify command: %q", v.Path)
	out, err := oswrap.Create(filepath.Join(dir, "googet_verify.log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	return goolib.Exec(filepath.Join(dir, v.Path), v.Args, v.ExitCodes, out)
}

// ObtainLock obtains a lock on a file path.
func ObtainLock(lockFile string) (func(), error) {
	err := os.MkdirAll(filepath.Dir(lockFile), 0755)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(lockFile, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}

	var cleanup func()
	c := make(chan error)
	go func() {
		cleanup, err = lock(f)
		c <- err
	}()

	ticker := time.NewTicker(5 * time.Second)
	// 90% of all GooGet runs happen in < 60s, we wait 70s.
	for i := 1; i < 15; i++ {
		select {
		case err := <-c:
			if err != nil {
				return nil, err
			}
			return cleanup, nil
		case <-ticker.C:
			fmt.Fprintln(os.Stdout, "GooGet lock already held, waiting...")
		}
	}
	return nil, errors.New("timed out waiting for lock")
}
