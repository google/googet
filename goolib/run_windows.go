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

package goolib

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/google/logger"
	"golang.org/x/sys/windows"
)

// startContained starts c in a Job Object that kills its processes when its
// last handle is closed, so they also die if googet dies. c is created
// suspended and resumed only once it is in the job, so no descendant starts
// outside it. kill terminates every process in the job and exited reports
// whether c itself has exited. release clears the kill-on-close limit and
// closes the job, so descendants left running after a normal exit keep
// running, as before. activity reads the job's and service counters and dialog
// looks for dialogs of its processes. If the job cannot be set up, for example
// because a parent job forbids it, kill only kills c and neither is available.
func startContained(c *exec.Cmd) (*contained, error) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := c.Start(); err != nil {
		return nil, err
	}
	pid := uint32(c.Process.Pid)
	// The process is suspended, so it cannot have exited and pid is valid.
	proc, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	var job windows.Handle
	var jobErr error
	if err == nil {
		job, jobErr = newJob(proc)
		err = resumeThreads(pid)
	}
	if err != nil {
		c.Process.Kill()
		c.Wait()
		for _, h := range []windows.Handle{proc, job} {
			if h != 0 {
				windows.CloseHandle(h)
			}
		}
		return nil, fmt.Errorf("starting %s: %w", c.Path, err)
	}
	exited := func() bool {
		ev, err := windows.WaitForSingleObject(proc, 0)
		return err == nil && ev == windows.WAIT_OBJECT_0
	}
	if jobErr != nil {
		logger.Warningf("Running %s without a job object, so only it will be killed on timeout and inactivity is not watched: %v", c.Path, jobErr)
		return &contained{
			kill:    func() { c.Process.Kill() },
			exited:  exited,
			release: func() { windows.CloseHandle(proc) },
		}, nil
	}
	return &contained{
		kill:   func() { windows.TerminateJobObject(job, 1) },
		exited: exited,
		release: func() {
			if err := setJobLimits(job, windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK); err != nil {
				logger.Warningf("Failed to clear the kill-on-close limit for %s; its remaining descendants will be killed: %v", c.Path, err)
			}
			windows.CloseHandle(job)
			windows.CloseHandle(proc)
		},
		activity: addActivity(func() (uint64, uint64, error) { return jobActivity(job) }, serviceActivity),
		dialog:   func() (string, bool) { return jobDialog(job) },
	}, nil
}

// jobAccounting mirrors JOBOBJECT_BASIC_AND_IO_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	TotalUserTime, TotalKernelTime                     int64
	ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses                uint32
	ActiveProcesses, TotalTerminatedProcesses          uint32
	IoInfo                                             windows.IO_COUNTERS
}

// The Win32 structure is 96 bytes; these fail to compile otherwise.
var (
	_ [96 - unsafe.Sizeof(jobAccounting{})]byte
	_ [unsafe.Sizeof(jobAccounting{}) - 96]byte
)

// jobActivity returns the CPU time of job and the sum of its I/O operation and
// transfer counts and number of processes ever started. Each only grows, so
// the sum changes whenever any of them does.
func jobActivity(job windows.Handle) (cpu, io uint64, err error) {
	var a jobAccounting
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAndIoAccountingInformation,
		uintptr(unsafe.Pointer(&a)), uint32(unsafe.Sizeof(a)), nil); err != nil {
		return 0, 0, err
	}
	return uint64(a.TotalUserTime + a.TotalKernelTime), uint64(a.TotalProcesses) + ioTotal(a.IoInfo), nil
}

// ioTotal returns the sum of the I/O operation and transfer counts in c.
func ioTotal(c windows.IO_COUNTERS) uint64 {
	return c.ReadOperationCount + c.WriteOperationCount + c.OtherOperationCount +
		c.ReadTransferCount + c.WriteTransferCount + c.OtherTransferCount
}

// serviceImages are the image names of the Windows Installer service, which
// runs custom actions as msiexec.exe too, and of servicing. Tests replace it.
var serviceImages = []string{"msiexec.exe", "TrustedInstaller.exe", "TiWorker.exe"}

// procGetProcessIoCounters is loaded by hand because x/sys/windows has no
// wrapper for GetProcessIoCounters.
var procGetProcessIoCounters = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIoCounters")

// serviceActivity returns the CPU time of the processes named in
// serviceImages and of the processes they start, transitively, and the sum of
// their I/O counts and their number, skipping those it cannot open. Processes
// come and go, so the sums can also shrink.
func serviceActivity() (cpu, io uint64, err error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(snap)
	var roots []uint32
	children := map[uint32][]uint32{}
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		children[e.ParentProcessID] = append(children[e.ParentProcessID], e.ProcessID)
		name := windows.UTF16ToString(e.ExeFile[:])
		if slices.ContainsFunc(serviceImages, func(s string) bool { return strings.EqualFold(s, name) }) {
			roots = append(roots, e.ProcessID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return 0, 0, err
	}
	for _, pid := range withDescendants(roots, children) {
		if c, n, ok := processActivity(pid); ok {
			// A process starting or exiting changes the count, which is a
			// change too.
			cpu, io = cpu+c, io+n+1
		}
	}
	return cpu, io, nil
}

// processActivity returns the CPU time and I/O count of process pid, and
// whether it could be opened.
func processActivity(pid uint32) (cpu, io uint64, ok bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, 0, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
		cpu = filetimeTicks(kernel) + filetimeTicks(user)
	}
	var c windows.IO_COUNTERS
	if r, _, _ := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&c))); r != 0 {
		io = ioTotal(c)
	}
	return cpu, io, true
}

// filetimeTicks returns a duration held in a Filetime, in 100ns ticks.
// Filetime.Nanoseconds does not fit, since it converts from the 1601 epoch.
func filetimeTicks(f windows.Filetime) uint64 {
	return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)
}

var (
	user32                        = windows.NewLazySystemDLL("user32.dll")
	procGetWindow                 = user32.NewProc("GetWindow")
	procGetWindowLongW            = user32.NewProc("GetWindowLongW")
	procGetWindowTextW            = user32.NewProc("GetWindowTextW")
	procGetProcessWindowStation   = user32.NewProc("GetProcessWindowStation")
	procGetUserObjectInformationW = user32.NewProc("GetUserObjectInformationW")
)

const (
	gwOwner    = 4                // GW_OWNER.
	uoiFlags   = 1                // UOI_FLAGS.
	wsfVisible = 1                // WSF_VISIBLE.
	gwlExStyle = ^uintptr(20 - 1) // GWL_EXSTYLE (-20), sign-extended.
)

// Callbacks made by windows.NewCallback are never freed and only a limited
// number can be made, so one package-level callback serves all calls and gets
// its state from these variables. EnumWindows calls it synchronously, so
// jobDialog holds enumMu throughout.
var (
	enumMu       sync.Mutex
	enumPIDs     = map[uint32]bool{}
	enumTitle    string
	enumFound    bool
	enumCallback = windows.NewCallback(enumWindow)
)

// jobDialog returns the title of a dialog of a process in job, and whether
// there is one. If the processes cannot be listed, it reports no dialog.
func jobDialog(job windows.Handle) (string, bool) {
	// JOBOBJECT_BASIC_PROCESS_ID_LIST. With ERROR_MORE_DATA, processes past
	// the first 64 are not looked at.
	var l struct {
		Assigned, Listed uint32
		PIDs             [64]uintptr
	}
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&l)), uint32(unsafe.Sizeof(l)), nil); err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
		return "", false
	}
	enumMu.Lock()
	defer enumMu.Unlock()
	clear(enumPIDs)
	for _, pid := range l.PIDs[:min(l.Listed, uint32(len(l.PIDs)))] {
		enumPIDs[uint32(pid)] = true
	}
	enumTitle, enumFound = "", false
	// EnumWindows returns an error when enumWindow stops it early, on
	// finding a dialog, so its error says nothing.
	windows.EnumWindows(enumCallback, nil)
	return enumTitle, enumFound
}

// enumWindow is the EnumWindows callback. It stops at the first dialog of a
// process in enumPIDs and records its title.
func enumWindow(h windows.HWND, _ uintptr) uintptr {
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(h, &pid); err != nil || !enumPIDs[pid] {
		return 1
	}
	// WS_VISIBLE is a style bit, set on shown windows even on a window station
	// nobody sees, so hidden helper windows are never taken for dialogs.
	if !windows.IsWindowVisible(h) {
		return 1
	}
	var class [16]uint16
	n, _ := windows.GetClassName(h, &class[0], int32(len(class)))
	owner, _, _ := procGetWindow.Call(uintptr(h), gwOwner)
	exStyle, _, _ := procGetWindowLongW.Call(uintptr(h), gwlExStyle)
	if !isDialogWindow(windows.UTF16ToString(class[:n]), owner != 0, uint32(exStyle)) {
		return 1
	}
	var title [128]uint16
	m, _, _ := procGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&title[0])), uintptr(len(title)))
	enumTitle, enumFound = windows.UTF16ToString(title[:m]), true
	return 0
}

// unattended reports whether nobody can answer a dialog because googet runs
// in session 0, where services run, or on a window station without a display.
func unattended() (bool, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil || session == 0 {
		return err == nil, err
	}
	ws, _, err := procGetProcessWindowStation.Call()
	if ws == 0 {
		return false, err
	}
	var f struct {
		inherit, reserved int32
		flags             uint32
	}
	if r, _, err := procGetUserObjectInformationW.Call(ws, uoiFlags, uintptr(unsafe.Pointer(&f)), unsafe.Sizeof(f), 0); r == 0 {
		return false, err
	}
	return f.flags&wsfVisible == 0, nil
}

// newJob creates a kill-on-close Job Object and assigns proc to it. Breakaway
// is allowed because bootstrappers that start children with
// CREATE_BREAKAWAY_FROM_JOB would otherwise fail with ACCESS_DENIED; such
// children explicitly leave the job and are not killed with it.
func newJob(proc windows.Handle) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("creating job object: %w", err)
	}
	if err := setJobLimits(job, windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE|windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("setting job object limits: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("assigning process to job object: %w", err)
	}
	return job, nil
}

// setJobLimits sets the basic limit flags of job.
func setJobLimits(job windows.Handle, flags uint32) error {
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: flags},
	}
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}

// resumeThreads resumes the threads of process pid, which was created
// suspended. Threads that cannot be resumed, such as ones injected by security
// software, are skipped; it fails unless some suspended thread was resumed.
func resumeThreads(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := false
	lastErr := fmt.Errorf("no suspended thread found in process %d", pid)
	for err := windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			lastErr = oerr
			continue
		}
		// ResumeThread returns the previous suspend count.
		n, rerr := windows.ResumeThread(th)
		windows.CloseHandle(th)
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if n > 0 {
			resumed = true
		}
	}
	if resumed {
		return nil
	}
	return lastErr
}
