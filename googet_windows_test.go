//go:build windows

/*
Copyright 2026 Google Inc. All Rights Reserved.
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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/system"
	"golang.org/x/sys/windows"
)

func TestRunPrepareRootBeforeSettingsInitWindows(t *testing.T) {
	if system.IsAdmin() != nil {
		if os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatal("this test needs an elevated process")
		}
		t.Skip("needs an elevated process on Windows")
	}

	t.Run("junction root does not read googet.conf", func(t *testing.T) {
		target := t.TempDir()
		if err := os.Mkdir(filepath.Join(target, "googet.conf"), 0755); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(t.TempDir(), "googet")
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", root, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J %q %q: %v\n%s", root, target, err, out)
		}
		code, out := runGooget(t, true, "-root", root, "-system_log=false", "-noconfirm", "installed", "-format=json")
		if code != 1 {
			t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
		}
		if !strings.Contains(out, "reparse point") {
			t.Errorf("output = %q, want reparse point error", out)
		}
		if strings.Contains(out, "panic") || strings.Contains(out, "Error unmarshalling conf file") {
			t.Errorf("output unexpectedly read googet.conf through junction:\n%s", out)
		}
	})

	t.Run("restricted root repaired before reading googet.conf", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		if err := os.Mkdir(root, 0755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = system.PrepareRoot(root, nil) })
		confPath := filepath.Join(root, "googet.conf")
		if err := os.WriteFile(confPath, []byte("progress: false\n"), 0644); err != nil {
			t.Fatal(err)
		}
		// Give root an empty protected DACL so googet.conf inherits no read
		// access until PrepareRoot repairs root and propagates the canonical
		// DACL.
		sd, err := windows.SecurityDescriptorFromString("D:PAI")
		if err != nil {
			t.Fatalf("SecurityDescriptorFromString: %v", err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatalf("sd.DACL(): %v", err)
		}
		if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			t.Fatalf("SetNamedSecurityInfo(%q): %v", root, err)
		}
		if _, err := os.ReadFile(confPath); err == nil {
			var sddl string
			if gotSD, gerr := windows.GetNamedSecurityInfo(confPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION); gerr == nil {
				sddl = gotSD.String()
			}
			t.Fatalf("precondition: %q is still readable after restricting root ACL (conf SDDL: %s)", confPath, sddl)
		}
		code, out := runGooget(t, true, "-root", root, "-system_log=false", "-noconfirm", "installed", "-format=json")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		if strings.Contains(out, "panic") || strings.Contains(out, "Error unmarshalling conf file") {
			t.Errorf("output unexpectedly reported conf error:\n%s", out)
		}
	})
}
