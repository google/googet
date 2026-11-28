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
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// landlockABI returns the Landlock ABI version that the kernel supports, or
// 0 if Landlock isn't available.
func landlockABI() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// writeConfinementSupported reports whether confineWritesAndExec can work.
func writeConfinementSupported() bool {
	return landlockABI() >= 1
}

// landlockWriteAccess returns every Landlock filesystem right that ABI
// version abi knows of and that changes the filesystem.
func landlockWriteAccess(abi int) uint64 {
	a := uint64(unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		a |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		a |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	return a
}

// confineWritesAndExec denies the calling thread every filesystem write
// outside the directories in allow, then replaces the process with a new run
// of the test binary with environment env. A Landlock domain applies to the
// thread that creates it and to everything that thread starts, including
// the new program image, so every thread of the new process is confined.
// It returns only on error.
func confineWritesAndExec(allow []string, env []string) error {
	runtime.LockOSThread()
	abi := landlockABI()
	if abi < 1 {
		return errors.New("Landlock is not available")
	}
	access := landlockWriteAccess(abi)
	attr := unix.LandlockRulesetAttr{Access_fs: access}
	// Pass only the size of Access_fs, which every ABI version accepts.
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr.Access_fs), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	defer unix.Close(int(fd))
	for _, dir := range allow {
		dfd, err := unix.Open(dir, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("opening %q: %w", dir, err)
		}
		rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(dfd)}
		_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)))
		unix.Close(dfd)
		if errno != 0 {
			return fmt.Errorf("landlock_add_rule for %q: %w", dir, errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return unix.Exec(exe, os.Args, env)
}
