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

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/google/logger"
	"golang.org/x/sys/windows"
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindow                = user32.NewProc("GetWindow")
	procGetWindowLongW           = user32.NewProc("GetWindowLongW")
	procGetProcessWindowStation  = user32.NewProc("GetProcessWindowStation")
	procGetUserObjectInformation = user32.NewProc("GetUserObjectInformationW")
	enumWindowsCallback          = windows.NewCallback(enumWindowsProc)
)

const (
	// dialogClassName is the window class of standard Win32 dialog boxes, including MessageBox.
	dialogClassName = "#32770"
	// gwOwner is the GetWindow command that retrieves the owner window.
	gwOwner = 4
	// wsExDlgModalFrame is the extended window style of windows with a modal dialog frame.
	wsExDlgModalFrame = 0x00000001
	// uoiFlags is the GetUserObjectInformation index that returns USEROBJECTFLAGS.
	uoiFlags = 1
	// wsfVisible is the USEROBJECTFLAGS flag of a window station that has visible display
	// surfaces, that is, an interactive window station.
	wsfVisible = 0x0001
)

// userObjectFlags mirrors Win32 USEROBJECTFLAGS from winuser.h.
type userObjectFlags struct {
	Inherit  int32
	Reserved int32
	Flags    uint32
}

// interactiveStation caches whether this process runs on an interactive window station.
var interactiveStation = sync.OnceValue(func() bool {
	ws, _, _ := procGetProcessWindowStation.Call()
	if ws == 0 {
		return true
	}
	var f userObjectFlags
	var needed uint32
	r, _, _ := procGetUserObjectInformation.Call(ws, uoiFlags, uintptr(unsafe.Pointer(&f)), unsafe.Sizeof(f), uintptr(unsafe.Pointer(&needed)))
	if r == 0 {
		// Assume interactive so that the visibility filter stays in place.
		return true
	}
	return f.Flags&wsfVisible != 0
})

// gwlExStyle is the GetWindowLong index of the extended window style. It is a variable because
// the negative constant cannot be converted to uintptr directly.
var gwlExStyle int32 = -20

// jobObjectBasicAccountingInformation mirrors Win32 JOBOBJECT_BASIC_ACCOUNTING_INFORMATION from winnt.h.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// jobObjectBasicAndIoAccountingInformation mirrors Win32 JOBOBJECT_BASIC_AND_IO_ACCOUNTING_INFORMATION from winnt.h.
type jobObjectBasicAndIoAccountingInformation struct {
	BasicInfo jobObjectBasicAccountingInformation
	IoInfo    windows.IO_COUNTERS
}

// jobObjectBasicProcessIDListHeader describes the fixed header of JOBOBJECT_BASIC_PROCESS_ID_LIST.
type jobObjectBasicProcessIDListHeader struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
}

// getJobPIDs returns all active process IDs assigned to the given Job Object plus rootPID.
func getJobPIDs(job windows.Handle, rootPID uint32) []uint32 {
	pidsMap := make(map[uint32]bool)
	if rootPID != 0 {
		pidsMap[rootPID] = true
	}
	if job != 0 {
		const initialCap = 64
		uintptrSize := int(unsafe.Sizeof(uintptr(0)))
		// The ULONG_PTR PID list follows the two DWORD header fields on both 32-bit and 64-bit.
		const listOffset = 8
		buf := make([]byte, listOffset+initialCap*uintptrSize)

		var retLen uint32
		err := windows.QueryInformationJobObject(
			job,
			int32(windows.JobObjectBasicProcessIdList),
			uintptr(unsafe.Pointer(&buf[0])),
			uint32(len(buf)),
			&retLen,
		)
		if err != nil && errors.Is(err, windows.ERROR_MORE_DATA) {
			header := (*jobObjectBasicProcessIDListHeader)(unsafe.Pointer(&buf[0]))
			needed := header.NumberOfAssignedProcesses
			if needed > 0 {
				buf = make([]byte, listOffset+int(needed+16)*uintptrSize)
				err = windows.QueryInformationJobObject(
					job,
					int32(windows.JobObjectBasicProcessIdList),
					uintptr(unsafe.Pointer(&buf[0])),
					uint32(len(buf)),
					&retLen,
				)
			}
		}
		if err == nil {
			header := (*jobObjectBasicProcessIDListHeader)(unsafe.Pointer(&buf[0]))
			numInList := int(header.NumberOfProcessIdsInList)
			for i := 0; i < numInList; i++ {
				pidOffset := listOffset + i*uintptrSize
				if pidOffset+uintptrSize > len(buf) {
					break
				}
				pidVal := *(*uintptr)(unsafe.Pointer(&buf[pidOffset]))
				if pidVal != 0 {
					pidsMap[uint32(pidVal)] = true
				}
			}
		}
	}
	pids := make([]uint32, 0, len(pidsMap))
	for pid := range pidsMap {
		pids = append(pids, pid)
	}
	return pids
}

// windowEnumContext passes Job Object process IDs and collects matching windows during enumeration.
type windowEnumContext struct {
	jobPIDs map[uint32]bool
	matches []windowInfo
}

// enumContexts maps integer handles passed through EnumWindows' lParam to their contexts. Passing
// an integer instead of a Go pointer avoids converting a uintptr back to unsafe.Pointer in the
// callback, the same approach as runtime/cgo.Handle.
var (
	enumContextsMu   sync.Mutex
	enumContexts     = make(map[uintptr]*windowEnumContext)
	nextEnumContextH uintptr
)

// registerEnumContext stores ctx and returns its handle.
func registerEnumContext(ctx *windowEnumContext) uintptr {
	enumContextsMu.Lock()
	defer enumContextsMu.Unlock()
	nextEnumContextH++
	if nextEnumContextH == 0 {
		nextEnumContextH++
	}
	enumContexts[nextEnumContextH] = ctx
	return nextEnumContextH
}

// lookupEnumContext returns the context registered under h, or nil.
func lookupEnumContext(h uintptr) *windowEnumContext {
	enumContextsMu.Lock()
	defer enumContextsMu.Unlock()
	return enumContexts[h]
}

// unregisterEnumContext removes the context registered under h.
func unregisterEnumContext(h uintptr) {
	enumContextsMu.Lock()
	defer enumContextsMu.Unlock()
	delete(enumContexts, h)
}

// isCandidateWindow reports whether a visible top-level window looks like an interactive prompt:
// a standard dialog box, or an owned window with a modal dialog frame.
func isCandidateWindow(className string, hasOwner bool, exStyle uint32) bool {
	if className == dialogClassName {
		return true
	}
	return hasOwner && exStyle&wsExDlgModalFrame != 0
}

// enumWindowsProc is the callback function for EnumWindows.
func enumWindowsProc(hwnd uintptr, lParam uintptr) uintptr {
	ctx := lookupEnumContext(lParam)
	if ctx == nil {
		return 0
	}
	h := windows.HWND(hwnd)
	var pid uint32
	tid, err := windows.GetWindowThreadProcessId(h, &pid)
	if tid == 0 || err != nil || !ctx.jobPIDs[pid] {
		return 1
	}
	// On a non-interactive window station, such as session 0 where googet runs as
	// SYSTEM, IsWindowVisible is false even for a dialog that is blocking on input, so
	// the visibility filter only applies on interactive stations.
	if interactiveStation() && !windows.IsWindowVisible(h) {
		return 1
	}

	var classBuf [256]uint16
	copied, err := windows.GetClassName(h, &classBuf[0], int32(len(classBuf)))
	if err != nil || copied == 0 {
		return 1
	}
	className := windows.UTF16ToString(classBuf[:copied])
	owner, _, _ := procGetWindow.Call(hwnd, gwOwner)
	exStyle, _, _ := procGetWindowLongW.Call(hwnd, uintptr(gwlExStyle))

	if isCandidateWindow(className, owner != 0, uint32(exStyle)) {
		ctx.matches = append(ctx.matches, windowInfo{
			PID:       pid,
			HWND:      hwnd,
			Title:     getWindowTitle(h),
			ClassName: className,
			ExePath:   getProcessImagePath(pid),
		})
	}
	return 1
}

// getWindowTitle retrieves the title text of the given window.
func getWindowTitle(hwnd windows.HWND) string {
	var buf [512]uint16
	r0, _, _ := procGetWindowTextW.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r0 > 0 {
		return windows.UTF16ToString(buf[:r0])
	}
	return ""
}

// getProcessImagePath retrieves the full executable image path for the given process ID.
func getProcessImagePath(pid uint32) string {
	hProc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(hProc)

	var buf [1024]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(hProc, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// isSession0 checks whether the current process is running in Windows Session 0.
func isSession0() bool {
	var sessionID uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &sessionID); err != nil {
		return false
	}
	return sessionID == 0
}

// detectWindowsWin32 enumerates top-level windows owned by pids and returns candidate
// interactive prompts. Invisible windows are skipped only on an interactive window station.
func detectWindowsWin32(pids []uint32) ([]windowInfo, error) {
	ctx := &windowEnumContext{jobPIDs: make(map[uint32]bool, len(pids))}
	for _, p := range pids {
		ctx.jobPIDs[p] = true
	}
	h := registerEnumContext(ctx)
	defer unregisterEnumContext(h)
	r1, _, e1 := procEnumWindows.Call(enumWindowsCallback, h)
	if r1 == 0 && e1 != nil && !errors.Is(e1, windows.ERROR_SUCCESS) {
		return ctx.matches, e1
	}
	return ctx.matches, nil
}

// filetimeDuration converts a FILETIME interval, in 100ns units, to a time.Duration.
func filetimeDuration(ft windows.Filetime) time.Duration {
	return time.Duration(uint64(ft.HighDateTime)<<32|uint64(ft.LowDateTime)) * 100
}

// sampleJob returns the cumulative CPU time and read plus write bytes of every process that has
// ever run in the job. OtherTransferCount is excluded because control I/O is not install progress.
func sampleJob(job windows.Handle) (counters, error) {
	var info jobObjectBasicAndIoAccountingInformation
	err := windows.QueryInformationJobObject(
		job,
		int32(windows.JobObjectBasicAndIoAccountingInformation),
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	)
	if err != nil {
		return counters{}, err
	}
	return counters{
		cpu: time.Duration(info.BasicInfo.TotalUserTime+info.BasicInfo.TotalKernelTime) * 100,
		io:  info.IoInfo.ReadTransferCount + info.IoInfo.WriteTransferCount,
	}, nil
}

// setJobLimitFlags sets the basic limit flags of the job.
func setJobLimitFlags(job windows.Handle, flags uint32) error {
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: flags,
		},
	}
	_, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	return err
}

// resumeProcessThreads resumes every thread owned by pid. It is used to start a process created
// with CREATE_SUSPENDED once it has been assigned to the job. Threads that cannot be opened or
// resumed, for example threads injected by security software, are skipped with a warning; it
// fails only if no thread was resumed.
func resumeProcessThreads(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("creating thread snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	var lastErr error
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			lastErr = fmt.Errorf("opening thread %d: %w", te.ThreadID, oerr)
			logger.Warningf("Skipping thread %d of PID %d: %v", te.ThreadID, pid, oerr)
			continue
		}
		_, rerr := windows.ResumeThread(th)
		windows.CloseHandle(th)
		if rerr != nil {
			lastErr = fmt.Errorf("resuming thread %d: %w", te.ThreadID, rerr)
			logger.Warningf("Failed to resume thread %d of PID %d: %v", te.ThreadID, pid, rerr)
			continue
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerating threads: %w", err)
	}
	if resumed == 0 {
		if lastErr != nil {
			return fmt.Errorf("no thread of process %d was resumed: %w", pid, lastErr)
		}
		return fmt.Errorf("no threads found for process %d", pid)
	}
	return nil
}

// createKillOnCloseJob creates a Job Object that kills its processes when its last handle closes.
func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("creating Windows Job Object: %w", err)
	}
	if err := setJobLimitFlags(job, windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("setting JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: %w", err)
	}
	return job, nil
}

// createJob creates the Job Object for a supervised install; tests replace it to simulate systems
// where job creation fails.
var createJob = createKillOnCloseJob

// jobTree supervises a Job Object on Windows, or only the root process when a job could not be
// set up.
type jobTree struct {
	c    *exec.Cmd
	opts Options
	pid  uint32
	// job is zero when only the root process is supervised.
	job   windows.Handle
	proc  windows.Handle
	start time.Time
	last  counters
	// checkedImages records PIDs whose image name has been inspected.
	checkedImages map[uint32]bool
	rootMsiexec   bool
	ranMsiexec    bool
	msi           *serviceMonitor
	// servicing accounts TrustedInstaller work for wusa and DISM; it is nil until needed.
	servicing *serviceMonitor
}

// processIDs returns the PIDs of the job's active processes, or only the root PID without a job.
func (t *jobTree) processIDs() []uint32 {
	return getJobPIDs(t.job, t.pid)
}

// noteImages inspects the image names of newly seen processes. An msiexec process enables the
// MSI post-abort wait, and a wusa or DISM process enables TrustedInstaller accounting.
func (t *jobTree) noteImages(pids []uint32) {
	for _, pid := range pids {
		if t.checkedImages[pid] {
			continue
		}
		name := imageBaseName(getProcessImagePath(pid))
		if name == "" {
			// Retry on the next poll; the process may not be queryable yet.
			continue
		}
		t.checkedImages[pid] = true
		switch name {
		case msiexecImage:
			t.ranMsiexec = true
		case wusaImage, dismImage:
			if t.servicing == nil {
				logger.Infof("Installer started %s (PID %d); accounting TrustedInstaller servicing work as progress.", name, pid)
				t.servicing = newTrustedInstallerMonitor(t.opts, t.start)
			}
		}
	}
}

// sample returns the job's counters plus the counters of service trees working for it.
func (t *jobTree) sample(now time.Time) progressSample {
	if t.job != 0 {
		if jc, err := sampleJob(t.job); err == nil {
			t.last = jc
		}
	} else if pc, _, ok := handleCounters(t.proc); ok {
		t.last = pc
	}
	t.noteImages(t.processIDs())
	s := progressSample{at: now, cpu: t.last.cpu, io: t.last.io}
	for _, m := range []*serviceMonitor{t.msi, t.servicing} {
		if m != nil {
			mc := m.sample(now)
			s.cpu += mc.cpu
			s.io += mc.io
		}
	}
	return s
}

// windows returns candidate interactive windows owned by the job's processes.
func (t *jobTree) windows() []windowInfo {
	pids := t.processIDs()
	var wins []windowInfo
	if t.opts.windowDetector != nil {
		wins, _ = t.opts.windowDetector(pids)
	} else {
		wins, _ = detectWindowsWin32(pids)
	}
	return wins
}

// terminate kills every process in the job, or the root process without a job. Service
// processes are never part of the job and are not affected.
func (t *jobTree) terminate() {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
		return
	}
	_ = t.c.Process.Kill()
}

// afterAbort waits for the Windows Installer transaction if the install ran msiexec.
// TrustedInstaller servicing is never waited for or terminated here.
func (t *jobTree) afterAbort() {
	if t.servicing != nil {
		logger.Warningf("Servicing work in %s may still be running; it is never terminated by the supervisor.", trustedInstallerServiceName)
	}
	if t.rootMsiexec || t.ranMsiexec {
		t.msi.afterMSIAbort(t.opts)
	}
}

// rootExited reports whether the root process has exited.
func (t *jobTree) rootExited() bool {
	ev, err := windows.WaitForSingleObject(t.proc, 0)
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// runSupervised executes a process within a Windows Job Object with forward progress, hard
// timeout and UI watchdog monitoring. If the job cannot be set up, for example on systems
// without nested job support, it logs a warning and supervises only the root process, with
// only the hard timeout enforced as described at fallbackOptions.
func runSupervised(c *exec.Cmd, opts Options, out io.Writer) error {
	devNull, err := setupStdio(c, out)
	if err != nil {
		return err
	}
	defer devNull.Close()

	job, err := createJob()
	if err != nil {
		logger.Warningf("%v; falling back to supervising the installer's root process only.", err)
	}
	// On abort paths closing the handle kills every remaining process through
	// KILL_ON_JOB_CLOSE. On a normal exit the limit is cleared first so that surviving
	// descendants such as tray apps and updaters keep running.
	releaseOnClose := false
	defer func() {
		if job == 0 {
			return
		}
		if releaseOnClose {
			if err := setJobLimitFlags(job, 0); err != nil {
				logger.Warningf("Failed to clear job limits before close; surviving installer descendants will be terminated: %v", err)
			}
		}
		windows.CloseHandle(job)
	}()

	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED

	if err := c.Start(); err != nil {
		return err
	}
	pid := uint32(c.Process.Pid)

	waitErr := make(chan error, 1)
	startFailed := func(step string, err error) error {
		_ = c.Process.Kill()
		go func() { waitErr <- c.Wait() }()
		select {
		case <-waitErr:
		case <-time.After(killWaitTimeout):
		}
		return fmt.Errorf("%s for PID %d: %w", step, pid, err)
	}

	const access = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE | windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION
	proc, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return startFailed("opening process handle", err)
	}
	defer windows.CloseHandle(proc)

	if job != 0 {
		if err := windows.AssignProcessToJobObject(job, proc); err != nil {
			logger.Warningf("Assigning PID %d to the Job Object failed; falling back to supervising the installer's root process only: %v", pid, err)
			windows.CloseHandle(job)
			job = 0
		}
	}
	if job == 0 && opts.Mode != ModeOff {
		logger.Warningf("No Job Object for PID %d: disabling the inactivity and UI watchdogs; the hard timeout %v still applies to the root process.", pid, opts.HardTimeout)
		opts = fallbackOptions(opts)
	}
	if err := resumeProcessThreads(pid); err != nil {
		if job != 0 {
			_ = windows.TerminateJobObject(job, 1)
		}
		return startFailed("resuming suspended process", err)
	}

	go func() { waitErr <- c.Wait() }()

	start := time.Now()
	tree := &jobTree{
		c:             c,
		opts:          opts,
		pid:           pid,
		job:           job,
		proc:          proc,
		start:         start,
		checkedImages: make(map[uint32]bool),
		rootMsiexec:   isMsiexecCommand(c.Path),
		// Any wrapper may drive msiexec, so service-side MSI work is always accounted while
		// the _MSIExecute mutex exists.
		msi: newMSIMonitor(opts, start),
	}
	if isServicingCommand(c.Path) {
		tree.servicing = newTrustedInstallerMonitor(opts, start)
	}
	ticker := time.NewTicker(opts.pollInterval)
	defer ticker.Stop()
	err = supervise(opts, waitErr, tree, start, ticker.C)
	// Every watchdog abort wraps ErrTerminated; only then must the remaining processes die with
	// the job.
	releaseOnClose = !errors.Is(err, ErrTerminated)
	return err
}
