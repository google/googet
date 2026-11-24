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
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// canonicalRootACEs are the access control entries install.ps1 sets on the
// GooGet root.
var canonicalRootACEs = []string{
	"(A;OICI;FA;;;BA)",
	"(A;OICI;FA;;;SY)",
	"(A;OICI;0x1200a9;;;BU)",
}

// requireElevated skips the test when the process is not elevated, except on
// GitHub Actions, where the runners are elevated and a skip would hide a
// failure.
func requireElevated(t *testing.T) {
	t.Helper()
	if windows.GetCurrentProcessToken().IsElevated() {
		return
	}
	if onGitHubActions() {
		t.Fatal("install.ps1 tests must run elevated on GitHub Actions")
	}
	t.Skip("install.ps1 tests require an elevated process")
}

// psQuote formats s as a PowerShell single-quoted string literal.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func powershell(args ...string) *exec.Cmd {
	return exec.Command("powershell.exe", append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}, args...)...)
}

func installScriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// runInstallScript runs install.ps1 with -File against root, leaving the
// machine environment unchanged.
func runInstallScript(t *testing.T, root string) (string, error) {
	t.Helper()
	out, err := powershell("-File", installScriptPath(t), "-GooGetRoot", root, "-SkipEnvironment").CombinedOutput()
	return string(out), err
}

// runInstallScriptCommand runs install.ps1 through -Command, as goolib.Exec
// does, against root, leaving the machine environment unchanged.
func runInstallScriptCommand(t *testing.T, root string) (string, error) {
	t.Helper()
	command := fmt.Sprintf("& %s -GooGetRoot %s -SkipEnvironment", psQuote(installScriptPath(t)), psQuote(root))
	out, err := powershell("-Command", command).CombinedOutput()
	return string(out), err
}

// ownerAndDACL returns the owner and DACL of path in SDDL form.
func ownerAndDACL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%q): %v", path, err)
	}
	return sd.String()
}

// parseOwnerAndDACL splits an SDDL string of the form O:<owner>D:<flags><aces>
// into its owner, DACL flags and access control entries.
func parseOwnerAndDACL(sddl string) (owner, flags string, aces []string, err error) {
	rest, ok := strings.CutPrefix(sddl, "O:")
	if !ok {
		return "", "", nil, fmt.Errorf("no owner in %q", sddl)
	}
	owner, dacl, ok := strings.Cut(rest, "D:")
	if !ok {
		return "", "", nil, fmt.Errorf("no DACL in %q", sddl)
	}
	flags, entries, _ := strings.Cut(dacl, "(")
	if entries != "" {
		for _, e := range strings.SplitAfter("("+entries, ")") {
			if e != "" {
				aces = append(aces, e)
			}
		}
	}
	return owner, flags, aces, nil
}

func checkCanonicalRoot(t *testing.T, root string) {
	t.Helper()
	sddl := ownerAndDACL(t, root)
	t.Logf("%s: %s", root, sddl)
	owner, flags, aces, err := parseOwnerAndDACL(sddl)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "BA" {
		t.Errorf("owner of %s = %q, want BA", root, owner)
	}
	if !strings.Contains(flags, "P") {
		t.Errorf("DACL flags of %s = %q, want protected (P)", root, flags)
	}
	slices.Sort(aces)
	want := slices.Sorted(slices.Values(canonicalRootACEs))
	if diff := cmp.Diff(want, aces); diff != "" {
		t.Errorf("DACL entries of %s mismatch (-want +got):\n%s", root, diff)
	}
}

// makeJunction creates a directory junction at link pointing to target and
// removes it before the enclosing temporary directories are removed.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v\n%s", link, target, err, out)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("removing junction %s: %v", link, err)
		}
	})
}

// makeSymlink creates a file symbolic link at link pointing to target and
// removes it before the enclosing temporary directories are removed.
func makeSymlink(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink %s %s: %v\n%s", link, target, err, out)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("removing symlink %s: %v", link, err)
		}
	})
}

func isReparsePoint(t *testing.T, path string) bool {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		t.Fatalf("GetFileAttributes(%q): %v", path, err)
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func TestInstallScriptCreatesProtectedRoot(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "GooGet")

	if out, err := runInstallScript(t, root); err != nil {
		t.Fatalf("install.ps1 failed: %v\n%s", err, out)
	}
	checkCanonicalRoot(t, root)
}

func TestInstallScriptRepairsInheritedACL(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "GooGet")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, flags, _, err := parseOwnerAndDACL(ownerAndDACL(t, root)); err != nil {
		t.Fatal(err)
	} else if strings.Contains(flags, "P") {
		t.Fatalf("precondition: DACL of new directory %s is already protected", root)
	}

	if out, err := runInstallScript(t, root); err != nil {
		t.Fatalf("install.ps1 failed: %v\n%s", err, out)
	}
	checkCanonicalRoot(t, root)

	// Running it again must succeed and leave the root canonical.
	if out, err := runInstallScriptCommand(t, root); err != nil {
		t.Fatalf("install.ps1 (-Command) failed: %v\n%s", err, out)
	}
	checkCanonicalRoot(t, root)
}

func TestInstallScriptRefusesJunction(t *testing.T) {
	requireElevated(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "GooGet")
	makeJunction(t, root, target)
	before := ownerAndDACL(t, target)

	for _, tc := range []struct {
		name string
		run  func(*testing.T, string) (string, error)
	}{
		{name: "File", run: runInstallScript},
		{name: "Command", run: runInstallScriptCommand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.run(t, root)
			if err == nil {
				t.Errorf("install.ps1 (-%s) succeeded on junction %s, want failure\n%s", tc.name, root, out)
			} else {
				t.Logf("install.ps1 (-%s): %v\n%s", tc.name, err, out)
				checkOutputContains(t, root, out, "not a regular directory")
			}
		})
	}

	if after := ownerAndDACL(t, target); after != before {
		t.Errorf("security descriptor of junction target changed:\nbefore: %s\nafter:  %s", before, after)
	}
	if !isReparsePoint(t, root) {
		t.Errorf("%s is no longer a junction", root)
	}
}

func TestInstallScriptSkipEnvironment(t *testing.T) {
	requireElevated(t)
	root := filepath.Join(t.TempDir(), "GooGet")
	rootBefore, pathBefore := rawMachineEnvironment(t)

	if out, err := runInstallScript(t, root); err != nil {
		t.Fatalf("install.ps1 failed: %v\n%s", err, out)
	}

	checkMachineEnvironmentUnchanged(t, rootBefore, pathBefore)
}

// onGitHubActions reports whether the tests run on a GitHub Actions runner,
// which is discarded after the job.
func onGitHubActions() bool {
	return os.Getenv("GITHUB_ACTIONS") != ""
}

// withPrivilege runs fn on a locked OS thread that impersonates the process
// with the named privilege enabled. The process token, and so the token of
// any child process, is left unchanged.
func withPrivilege(t *testing.T, name string, fn func()) {
	t.Helper()
	runtime.LockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("ImpersonateSelf: %v", err)
	}
	defer func() {
		if err := windows.RevertToSelf(); err != nil {
			// The thread stays locked so that it exits with the goroutine.
			t.Errorf("RevertToSelf: %v", err)
			return
		}
		runtime.UnlockOSThread()
	}()
	var token windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, false, &token); err != nil {
		t.Fatalf("OpenThreadToken: %v", err)
	}
	defer token.Close()
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
		t.Fatalf("LookupPrivilegeValue(%s): %v", name, err)
	}
	privileges := windows.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges:     [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}},
	}
	if err := windows.AdjustTokenPrivileges(token, false, &privileges, 0, nil, nil); err != nil {
		t.Fatalf("AdjustTokenPrivileges(%s): %v", name, err)
	}
	fn()
}

// setSecurity sets the owner and, if dacl is not empty, the DACL of path from
// SDDL, for example setSecurity(t, path, "BU", "D:P(A;;FA;;;BU)"). A DACL with
// the P flag, or a NULL DACL ("D:NO_ACCESS_CONTROL"), is set as protected.
// SeRestorePrivilege is enabled so that any owner can be set.
func setSecurity(t *testing.T, path, owner, dacl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("O:" + owner + dacl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString(%q): %v", "O:"+owner+dacl, err)
	}
	ownerSID, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION)
	var acl *windows.ACL
	if dacl != "" {
		if acl, _, err = sd.DACL(); err != nil {
			t.Fatal(err)
		}
		control, _, err := sd.Control()
		if err != nil {
			t.Fatal(err)
		}
		info |= windows.DACL_SECURITY_INFORMATION
		if acl == nil || control&windows.SE_DACL_PROTECTED != 0 {
			info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
		} else {
			info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
		}
	}
	withPrivilege(t, "SeRestorePrivilege", func() {
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, ownerSID, nil, acl, nil); err != nil {
			t.Fatalf("SetNamedSecurityInfo(%q, %q): %v", path, "O:"+owner+dacl, err)
		}
	})
}

// resetSecurityOnCleanup gives path back to Administrators with a NULL DACL
// before the temporary directories are removed.
func resetSecurityOnCleanup(t *testing.T, path string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			setSecurity(t, path, "BA", "D:NO_ACCESS_CONTROL")
		}
	})
}

// registryValue is a registry string value and its type.
type registryValue struct {
	value  string
	kind   uint32
	exists bool
}

const machineEnvironmentKey = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`

// rawMachineEnvironment returns the unexpanded machine GooGetRoot and Path.
func rawMachineEnvironment(t *testing.T) (googetRoot, path registryValue) {
	t.Helper()
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvironmentKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	read := func(name string) registryValue {
		v, kind, err := k.GetStringValue(name)
		if errors.Is(err, registry.ErrNotExist) {
			return registryValue{}
		}
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return registryValue{value: v, kind: kind, exists: true}
	}
	return read("GooGetRoot"), read("Path")
}

// restoreMachineEnvironmentOnCleanup writes back the machine GooGetRoot and
// Path read now when the test ends.
func restoreMachineEnvironmentOnCleanup(t *testing.T) {
	t.Helper()
	googetRoot, path := rawMachineEnvironment(t)
	t.Cleanup(func() {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvironmentKey, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Close()
		for _, entry := range []struct {
			name string
			v    registryValue
		}{
			{name: "GooGetRoot", v: googetRoot},
			{name: "Path", v: path},
		} {
			switch {
			case !entry.v.exists:
				err = k.DeleteValue(entry.name)
				if errors.Is(err, registry.ErrNotExist) {
					err = nil
				}
			case entry.v.kind == registry.EXPAND_SZ:
				err = k.SetExpandStringValue(entry.name, entry.v.value)
			default:
				err = k.SetStringValue(entry.name, entry.v.value)
			}
			if err != nil {
				t.Errorf("restoring machine %s: %v", entry.name, err)
			}
		}
	})
}

func checkMachineEnvironmentUnchanged(t *testing.T, rootBefore, pathBefore registryValue) {
	t.Helper()
	rootAfter, pathAfter := rawMachineEnvironment(t)
	if diff := cmp.Diff(rootBefore, rootAfter, cmp.AllowUnexported(registryValue{})); diff != "" {
		t.Errorf("machine GooGetRoot changed (-before +after):\n%s", diff)
	}
	if diff := cmp.Diff(pathBefore, pathAfter, cmp.AllowUnexported(registryValue{})); diff != "" {
		t.Errorf("machine Path changed (-before +after):\n%s", diff)
	}
}

// checkOutputContains verifies that out contains want after collapsing
// PowerShell console line wrapping.
func checkOutputContains(t *testing.T, target, out, want string) {
	t.Helper()
	normalized := strings.Join(strings.Fields(out), " ")
	if !strings.Contains(normalized, want) {
		t.Errorf("install.ps1 output on %s missing %q:\n%s", target, want, out)
	}
}

// runInstallScriptExpectingFailure runs install.ps1 against root, expects it
// to fail with output containing wantErr, and checks that the machine
// GooGetRoot and Path are unchanged. On GitHub Actions the script runs without
// -SkipEnvironment, so the check covers the order of its steps; elsewhere it
// runs with -SkipEnvironment.
func runInstallScriptExpectingFailure(t *testing.T, root, wantErr string) {
	t.Helper()
	if wantErr == "" {
		t.Fatal("wantErr must not be empty")
	}
	args := []string{"-File", installScriptPath(t), "-GooGetRoot", root}
	if onGitHubActions() {
		restoreMachineEnvironmentOnCleanup(t)
	} else {
		args = append(args, "-SkipEnvironment")
	}
	rootBefore, pathBefore := rawMachineEnvironment(t)
	rawOut, err := powershell(args...).CombinedOutput()
	out := string(rawOut)
	if err == nil {
		t.Errorf("install.ps1 succeeded on %s, want failure\n%s", root, out)
	} else {
		t.Logf("install.ps1: %v\n%s", err, out)
		checkOutputContains(t, root, out, wantErr)
	}
	checkMachineEnvironmentUnchanged(t, rootBefore, pathBefore)
}

func TestInstallScriptRepairsUntrustedRoot(t *testing.T) {
	requireElevated(t)
	for _, tc := range []struct {
		name, owner, dacl string
	}{
		{name: "UsersOwnerInheritedDACL", owner: "BU"},
		{name: "ExplicitUsersWriteEntry", owner: "BU", dacl: "D:(A;OICI;0x1301bf;;;BU)"},
		{name: "ProtectedUsersFullControl", owner: "BU", dacl: "D:P(A;OICI;FA;;;BU)(A;OICI;FA;;;BA)(A;OICI;FA;;;WD)"},
		{name: "OwnerRightsAndCreatorOwner", owner: "BU", dacl: "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)(A;OICIIO;FA;;;CO)"},
		{name: "NullDACL", owner: "BU", dacl: "D:NO_ACCESS_CONTROL"},
		{name: "AdministratorsReadOnly", owner: "BU", dacl: "D:P(A;;0x1200a9;;;BA)(A;;0x1200a9;;;SY)(A;;FA;;;BU)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "GooGet")
			if err := os.Mkdir(root, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "googet.exe"), nil, 0644); err != nil {
				t.Fatal(err)
			}
			resetSecurityOnCleanup(t, root)
			setSecurity(t, root, tc.owner, tc.dacl)
			t.Logf("before: %s", ownerAndDACL(t, root))

			if out, err := runInstallScript(t, root); err != nil {
				t.Fatalf("install.ps1 failed: %v\n%s", err, out)
			}
			checkCanonicalRoot(t, root)
		})
	}
}

// longPath converts any 8.3 short names in path to their long form.
func longPath(t *testing.T, path string) string {
	t.Helper()
	long := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(windows.StringToUTF16Ptr(path), &long[0], uint32(len(long)))
	if err != nil {
		t.Fatalf("GetLongPathName(%q): %v", path, err)
	}
	return windows.UTF16ToString(long[:n])
}

// mountFAT32Volume creates a FAT32 virtual disk in dir, mounts it and returns
// its root directory. It runs only on GitHub Actions because it changes the
// volumes of the machine.
func mountFAT32Volume(t *testing.T, dir string) string {
	t.Helper()
	if !onGitHubActions() {
		t.Skip("mounts a virtual disk; runs only on GitHub Actions")
	}
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	letter := byte(0)
	for l := byte('Z'); l >= 'G'; l-- {
		if drives&(1<<(l-'A')) == 0 {
			letter = l
			break
		}
	}
	if letter == 0 {
		t.Fatal("no free drive letter")
	}
	// diskpart identifies an attached virtual disk by its long path, and
	// TEMP can contain 8.3 short names.
	dir = longPath(t, dir)
	vhd := filepath.Join(dir, "fat32.vhd")
	runDiskpart := func(scriptName string, commands ...string) (string, error) {
		script := filepath.Join(dir, scriptName)
		if err := os.WriteFile(script, []byte(strings.Join(commands, "\r\n")+"\r\n"), 0644); err != nil {
			return "", err
		}
		out, err := exec.Command("diskpart.exe", "/s", script).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() {
		if _, err := os.Stat(vhd); errors.Is(err, os.ErrNotExist) {
			return
		}
		if out, err := runDiskpart("diskpart-detach.txt",
			fmt.Sprintf(`select vdisk file="%s"`, vhd),
			"detach vdisk",
		); err != nil {
			t.Errorf("detaching vdisk %s: %v\n%s", vhd, err, out)
		}
	})
	if out, err := runDiskpart("diskpart-attach.txt",
		fmt.Sprintf(`create vdisk file="%s" maximum=64 type=expandable`, vhd),
		fmt.Sprintf(`select vdisk file="%s"`, vhd),
		"attach vdisk",
		"create partition primary",
		"format fs=fat32 quick label=GOOGETTEST",
		fmt.Sprintf("assign letter=%c", letter),
	); err != nil {
		t.Fatalf("diskpart attach %s: %v\n%s", vhd, err, out)
	}
	return fmt.Sprintf("%c:\\", letter)
}

func TestInstallScriptRefusesUnusableRoot(t *testing.T) {
	requireElevated(t)
	for _, tc := range []struct {
		name string
		// wantErr is a locale-independent substring expected in the failure output.
		wantErr string
		// setup creates the fixture under dir and returns the root and the
		// paths whose security descriptors must not change.
		setup func(t *testing.T, dir string) (root string, unchanged []string)
	}{
		{
			name:    "Whitespace",
			wantErr: "GooGetRoot must not be empty",
			setup: func(t *testing.T, dir string) (string, []string) {
				return "   ", nil
			},
		},
		{
			name:    "RelativeDot",
			wantErr: "must be an absolute path",
			setup: func(t *testing.T, dir string) (string, []string) {
				return ".", nil
			},
		},
		{
			name:    "RelativeSubdir",
			wantErr: "must be an absolute path",
			setup: func(t *testing.T, dir string) (string, []string) {
				return `.\GooGet`, nil
			},
		},
		{
			name:    "DriveRelative",
			wantErr: "must be an absolute path",
			setup: func(t *testing.T, dir string) (string, []string) {
				return "C:GooGet", nil
			},
		},
		{
			name:    "RootRelative",
			wantErr: "must be an absolute path",
			setup: func(t *testing.T, dir string) (string, []string) {
				return `\GooGet`, nil
			},
		},
		{
			name:    "ExtendedLengthDevicePrefix",
			wantErr: "must be an absolute path",
			setup: func(t *testing.T, dir string) (string, []string) {
				return `\\?\C:\GooGet`, nil
			},
		},
		{
			name:    "LocalDevicePrefix",
			wantErr: "must be an absolute path",
			setup: func(t *testing.T, dir string) (string, []string) {
				return `\\.\C:\GooGet`, nil
			},
		},
		{
			name:    "UNCPathWithDotDotServer",
			wantErr: "must be an absolute path",
			setup: func(t *testing.T, dir string) (string, []string) {
				return `\\..\GooGet`, nil
			},
		},
		{
			name:    "UNCPathWithSlashServer",
			wantErr: "must be an absolute path",
			setup: func(t *testing.T, dir string) (string, []string) {
				return `\\/\GooGet`, nil
			},
		},
		{
			name:    "VolumeRoot",
			wantErr: "volume root",
			setup: func(t *testing.T, dir string) (string, []string) {
				return `C:\`, []string{`C:\`}
			},
		},
		{
			name:    "UNCShareRoot",
			wantErr: "volume root",
			setup: func(t *testing.T, dir string) (string, []string) {
				return `\\server\share`, nil
			},
		},
		{
			name:    "File",
			wantErr: "not a regular directory",
			setup: func(t *testing.T, dir string) (string, []string) {
				root := filepath.Join(dir, "GooGet")
				if err := os.WriteFile(root, []byte("x"), 0644); err != nil {
					t.Fatal(err)
				}
				return root, []string{root}
			},
		},
		{
			name:    "Junction",
			wantErr: "not a regular directory",
			setup: func(t *testing.T, dir string) (string, []string) {
				target := filepath.Join(dir, "target")
				if err := os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(dir, "GooGet")
				makeJunction(t, root, target)
				return root, []string{target}
			},
		},
		{
			name:    "JunctionTrailingBackslash",
			wantErr: "not a regular directory",
			setup: func(t *testing.T, dir string) (string, []string) {
				target := filepath.Join(dir, "target")
				if err := os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(dir, "GooGet")
				makeJunction(t, root, target)
				return root + `\`, []string{target}
			},
		},
		{
			name:    "DanglingJunction",
			wantErr: "not a regular directory",
			setup: func(t *testing.T, dir string) (string, []string) {
				root := filepath.Join(dir, "GooGet")
				makeJunction(t, root, filepath.Join(dir, "missing"))
				return root, nil
			},
		},
		{
			name:    "GooGetExeDirectory",
			wantErr: "not a regular file",
			setup: func(t *testing.T, dir string) (string, []string) {
				root := filepath.Join(dir, "GooGet")
				if err := os.MkdirAll(filepath.Join(root, "googet.exe"), 0755); err != nil {
					t.Fatal(err)
				}
				return root, []string{root}
			},
		},
		{
			name:    "GooGetExeJunction",
			wantErr: "not a regular file",
			setup: func(t *testing.T, dir string) (string, []string) {
				target := filepath.Join(dir, "target")
				if err := os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(dir, "GooGet")
				if err := os.Mkdir(root, 0755); err != nil {
					t.Fatal(err)
				}
				makeJunction(t, filepath.Join(root, "googet.exe"), target)
				return root, []string{root, target}
			},
		},
		{
			name:    "GooGetExeDanglingJunction",
			wantErr: "not a regular file",
			setup: func(t *testing.T, dir string) (string, []string) {
				root := filepath.Join(dir, "GooGet")
				if err := os.Mkdir(root, 0755); err != nil {
					t.Fatal(err)
				}
				makeJunction(t, filepath.Join(root, "googet.exe"), filepath.Join(dir, "missing"))
				return root, []string{root}
			},
		},
		{
			name:    "GooGetExeDanglingSymlink",
			wantErr: "not a regular file",
			setup: func(t *testing.T, dir string) (string, []string) {
				root := filepath.Join(dir, "GooGet")
				if err := os.Mkdir(root, 0755); err != nil {
					t.Fatal(err)
				}
				makeSymlink(t, filepath.Join(root, "googet.exe"), filepath.Join(dir, "missing"))
				return root, []string{root}
			},
		},
		{
			name:    "GooGetExeInUnreadableRoot",
			wantErr: "UnauthorizedAccessException",
			setup: func(t *testing.T, dir string) (string, []string) {
				root := filepath.Join(dir, "GooGet")
				if err := os.Mkdir(root, 0755); err != nil {
					t.Fatal(err)
				}
				googetExe := filepath.Join(root, "googet.exe")
				if err := os.WriteFile(googetExe, nil, 0644); err != nil {
					t.Fatal(err)
				}
				// Register root cleanup after googet.exe so LIFO cleanup
				// restores root first and makes googet.exe reachable.
				resetSecurityOnCleanup(t, googetExe)
				resetSecurityOnCleanup(t, root)
				setSecurity(t, googetExe, "BU", "D:P(D;;FA;;;BA)(A;;FA;;;BU)")
				setSecurity(t, root, "BU", "D:P(D;;FA;;;BA)(A;;FA;;;BU)")
				return root, nil
			},
		},
		{
			// The ACL steps fail on a volume without persistent ACLs.
			name:    "FAT32Volume",
			wantErr: "were not applied",
			setup: func(t *testing.T, dir string) (string, []string) {
				return filepath.Join(mountFAT32Volume(t, dir), "GooGet"), nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			root, unchanged := tc.setup(t, dir)
			before := map[string]string{}
			for _, p := range unchanged {
				before[p] = ownerAndDACL(t, p)
			}

			runInstallScriptExpectingFailure(t, root, tc.wantErr)

			for _, p := range unchanged {
				if after := ownerAndDACL(t, p); after != before[p] {
					t.Errorf("security descriptor of %s changed:\nbefore: %s\nafter:  %s", p, before[p], after)
				}
			}
			if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Lstat(%s) = %v, want not exist", filepath.Join(dir, "missing"), err)
			}
		})
	}
}

func TestInstallScriptLeavesJunctionTargetUnderRootUnchanged(t *testing.T) {
	requireElevated(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(target, "file.txt")
	if err := os.WriteFile(targetFile, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "GooGet")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, filepath.Join(root, "repos"), target)
	checked := []string{target, targetFile}
	before := map[string]string{target: ownerAndDACL(t, target), targetFile: ownerAndDACL(t, targetFile)}

	if out, err := runInstallScript(t, root); err != nil {
		t.Fatalf("install.ps1 failed: %v\n%s", err, out)
	}
	checkCanonicalRoot(t, root)

	for _, p := range checked {
		if after := ownerAndDACL(t, p); after != before[p] {
			t.Errorf("security descriptor of %s changed:\nbefore: %s\nafter:  %s", p, before[p], after)
		}
	}
}

func TestInstallScriptRootWithSpaces(t *testing.T) {
	requireElevated(t)
	for _, tc := range []struct {
		name     string
		run      func(*testing.T, string) (string, error)
		suffix   string
		existing bool
	}{
		{name: "File", run: runInstallScript},
		{name: "Command", run: runInstallScriptCommand},
		{name: "FileTrailingBackslash", run: runInstallScript, suffix: `\`},
		{name: "CommandTrailingBackslash", run: runInstallScriptCommand, suffix: `\`},
		{name: "ExistingTrailingBackslash", run: runInstallScript, suffix: `\`, existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "Goo Get Root")
			if tc.existing {
				if err := os.Mkdir(root, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := tc.run(t, root+tc.suffix); err != nil {
				t.Fatalf("install.ps1 failed: %v\n%s", err, out)
			}
			checkCanonicalRoot(t, root)
		})
	}
}

func TestInstallScriptSetsEnvironment(t *testing.T) {
	requireElevated(t)
	if !onGitHubActions() {
		t.Skip("changes the machine environment; runs only on GitHub Actions")
	}
	restoreMachineEnvironmentOnCleanup(t)
	_, pathBefore := rawMachineEnvironment(t)
	// Resolve 8.3 short names in TEMP because .NET Path.GetFullPath expands
	// them on Windows PowerShell 5.1.
	root := filepath.Join(longPath(t, t.TempDir()), "Goo Get")

	if out, err := powershell("-File", installScriptPath(t), "-GooGetRoot", root+`\`).CombinedOutput(); err != nil {
		t.Fatalf("install.ps1 failed: %v\n%s", err, out)
	}
	checkCanonicalRoot(t, root)
	googetRoot, path := rawMachineEnvironment(t)
	if googetRoot.value != root {
		t.Errorf("machine GooGetRoot = %q, want %q", googetRoot.value, root)
	}
	if !slices.Contains(strings.Split(path.value, ";"), root) {
		t.Errorf("machine Path = %q, want it to contain %q", path.value, root)
	}
	t.Logf("machine Path type before: %d, after: %d", pathBefore.kind, path.kind)
}

func TestInstallScriptFromPowerShell7(t *testing.T) {
	requireElevated(t)
	pwsh, err := exec.LookPath("pwsh.exe")
	if err != nil {
		if onGitHubActions() {
			t.Fatalf("pwsh.exe not found on GitHub Actions: %v", err)
		}
		t.Skip("pwsh.exe not found")
	}

	t.Run("WindowsPowerShellFromPwsh", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "GooGet")
		if err := os.Mkdir(root, 0755); err != nil {
			t.Fatal(err)
		}
		resetSecurityOnCleanup(t, root)
		setSecurity(t, root, "BU", "")
		command := fmt.Sprintf(
			"powershell.exe -NoProfile -NonInteractive -Command 'Write-Output \"PSModulePath: $env:PSModulePath\"'; "+
				"powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File %s -GooGetRoot %s -SkipEnvironment; "+
				"exit $LASTEXITCODE", psQuote(installScriptPath(t)), psQuote(root))
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
		t.Logf("pwsh: %v\n%s", err, out)
		if err != nil {
			t.Fatalf("install.ps1 started from pwsh failed: %v", err)
		}
		checkCanonicalRoot(t, root)
	})

	t.Run("DirectCreate", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "GooGet")
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-File", installScriptPath(t), "-GooGetRoot", root, "-SkipEnvironment").CombinedOutput()
		if err != nil {
			t.Fatalf("pwsh install.ps1 failed: %v\n%s", err, out)
		}
		checkCanonicalRoot(t, root)
	})

	t.Run("DirectRepair", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "GooGet")
		if err := os.Mkdir(root, 0755); err != nil {
			t.Fatal(err)
		}
		resetSecurityOnCleanup(t, root)
		setSecurity(t, root, "BU", "")
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-File", installScriptPath(t), "-GooGetRoot", root, "-SkipEnvironment").CombinedOutput()
		if err != nil {
			t.Fatalf("pwsh install.ps1 failed: %v\n%s", err, out)
		}
		checkCanonicalRoot(t, root)
	})
}

func TestInstallScriptThroughExec(t *testing.T) {
	requireElevated(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "GooGet")
	var out bytes.Buffer
	if err := goolib.Exec(installScriptPath(t), []string{"-GooGetRoot", root, "-SkipEnvironment"}, nil, &out); err != nil {
		t.Fatalf("goolib.Exec(install.ps1) on a new root failed: %v\n%s", err, out.String())
	}
	checkCanonicalRoot(t, root)

	junction := filepath.Join(dir, "junction")
	makeJunction(t, junction, root)
	out.Reset()
	if err := goolib.Exec(installScriptPath(t), []string{"-GooGetRoot", junction, "-SkipEnvironment"}, nil, &out); err == nil {
		t.Errorf("goolib.Exec(install.ps1) on junction %s succeeded, want failure\n%s", junction, out.String())
	} else {
		checkOutputContains(t, junction, out.String(), "not a regular directory")
	}
}

// readBackProblem dot-sources install.ps1 without running its main block and
// returns what Get-GooGetRootAclProblem reports for path.
func readBackProblem(t *testing.T, path string) string {
	t.Helper()
	command := fmt.Sprintf("$ErrorActionPreference = 'Stop'; . %s; Write-Output ('PROBLEM:' + (Get-GooGetRootAclProblem %s))",
		psQuote(installScriptPath(t)), psQuote(path))
	out, err := powershell("-Command", command).CombinedOutput()
	if err != nil {
		t.Fatalf("Get-GooGetRootAclProblem(%s): %v\n%s", path, err, out)
	}
	_, problem, ok := strings.Cut(strings.TrimSpace(string(out)), "PROBLEM:")
	if !ok {
		t.Fatalf("Get-GooGetRootAclProblem(%s): no result in output:\n%s", path, out)
	}
	return problem
}

func TestInstallScriptReadBackDetectsDifferences(t *testing.T) {
	requireElevated(t)
	const canonicalDACL = "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)"
	for _, tc := range []struct {
		name, owner, dacl string
		// want is a substring of the reported problem, or empty for none.
		want string
	}{
		{name: "Canonical", owner: "BA", dacl: canonicalDACL},
		{name: "UsersOwner", owner: "BU", dacl: canonicalDACL, want: "owner is S-1-5-32-545"},
		{name: "SystemOwner", owner: "SY", dacl: canonicalDACL, want: "owner is S-1-5-18"},
		{name: "NotProtected", owner: "BA", dacl: "D:(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)", want: "not protected"},
		// .NET reports a NULL DACL as a single Everyone full control entry.
		{name: "NullDACL", owner: "BA", dacl: "D:NO_ACCESS_CONTROL", want: "DACL has 1 entries"},
		{name: "ExtraReadEntry", owner: "BA", dacl: canonicalDACL + "(A;OICI;0x1200a9;;;WD)", want: "DACL has 4 entries"},
		{name: "UsersModify", owner: "BA", dacl: "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;BU)", want: "unexpected entry for S-1-5-32-545"},
		{name: "UsersNotObjectInherit", owner: "BA", dacl: "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;CI;0x1200a9;;;BU)", want: "unexpected entry for S-1-5-32-545"},
		{name: "UsersInheritOnly", owner: "BA", dacl: "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICIIO;0x1200a9;;;BU)", want: "unexpected entry for S-1-5-32-545"},
		{name: "UsersDeny", owner: "BA", dacl: "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(D;OICI;0x1200a9;;;BU)", want: "unexpected entry for S-1-5-32-545"},
		{name: "EveryoneInsteadOfUsers", owner: "BA", dacl: "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;WD)", want: "unexpected entry for S-1-1-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "GooGet")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			resetSecurityOnCleanup(t, dir)
			setSecurity(t, dir, tc.owner, tc.dacl)

			got := readBackProblem(t, dir)
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("Get-GooGetRootAclProblem(%s) = %q, want %q (descriptor %s)", tc.name, got, tc.want, ownerAndDACL(t, dir))
			}
		})
	}
}
