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
	"time"
	"unsafe"

	"github.com/google/logger"
	"golang.org/x/sys/windows"
)

const (
	// msiServiceName is the service name of the Windows Installer service.
	msiServiceName = "msiserver"
	// trustedInstallerServiceName is the service name of the Windows Modules Installer, which
	// performs servicing for wusa and DISM.
	trustedInstallerServiceName = "TrustedInstaller"
	// creationTimeSlack tolerates clock granularity when comparing creation times with the
	// supervision start.
	creationTimeSlack = 2 * time.Second
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessIoCounters = kernel32.NewProc("GetProcessIoCounters")
)

// mutexExists reports whether a named mutex exists. ERROR_ACCESS_DENIED means the mutex exists
// but its DACL does not grant SYNCHRONIZE to this process.
func mutexExists(name string) bool {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, p)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	windows.CloseHandle(h)
	return true
}

// msiMutexHeld reports whether a Windows Installer transaction is in progress.
func msiMutexHeld() bool {
	return mutexExists(msiMutexName)
}

// serviceProcessID returns the PID of the named service, or 0 if it is not running.
func serviceProcessID(service string) (uint32, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(scm)
	name, err := windows.UTF16PtrFromString(service)
	if err != nil {
		return 0, err
	}
	svc, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(svc)
	var status windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(svc, windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed); err != nil {
		return 0, err
	}
	return status.ProcessId, nil
}

// snapshotProcesses returns the current process table.
func snapshotProcesses() ([]procEntry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var entries []procEntry
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		entries = append(entries, procEntry{
			pid:  pe.ProcessID,
			ppid: pe.ParentProcessID,
			exe:  windows.UTF16ToString(pe.ExeFile[:]),
		})
	}
	return entries, nil
}

// handleCounters returns cumulative CPU time, read plus write bytes and the creation time of the
// process behind h, which needs PROCESS_QUERY_LIMITED_INFORMATION access.
func handleCounters(h windows.Handle) (counters, time.Time, bool) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return counters{}, time.Time{}, false
	}
	c := counters{cpu: filetimeDuration(kernel) + filetimeDuration(user)}
	var ioc windows.IO_COUNTERS
	if r1, _, _ := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&ioc))); r1 != 0 {
		c.io = ioc.ReadTransferCount + ioc.WriteTransferCount
	}
	return c, time.Unix(0, creation.Nanoseconds()), true
}

// processCounters returns cumulative CPU time, read plus write bytes and the creation time of pid.
func processCounters(pid uint32) (counters, time.Time, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return counters{}, time.Time{}, false
	}
	defer windows.CloseHandle(h)
	return handleCounters(h)
}

// serviceTreeSample is one observation of a service process tree.
type serviceTreeSample struct {
	perPID     map[procKey]counters
	terminable []uint32
}

// sampleServiceTree measures the service process, every process whose image is one of
// extraRoots, and all of their transitive descendants. If withTerminable is set, it also lists
// as terminable the descendants created at or after notBefore, never a root.
func sampleServiceTree(service string, extraRoots []string, withTerminable bool, notBefore time.Time) serviceTreeSample {
	out := serviceTreeSample{perPID: make(map[procKey]counters)}
	svc, err := serviceProcessID(service)
	if err != nil {
		svc = 0
	}
	entries, err := snapshotProcesses()
	if err != nil {
		return out
	}
	roots := []uint32{svc}
	for _, image := range extraRoots {
		roots = append(roots, pidsWithImage(entries, image)...)
	}
	created := make(map[uint32]time.Time)
	measured := make(map[uint32]bool)
	// createdAt measures pid on first use: it records the process's counters in out.perPID,
	// keyed on its PID and creation time, and memoizes the creation time.
	createdAt := func(pid uint32) (time.Time, bool) {
		if !measured[pid] {
			measured[pid] = true
			if c, t, ok := processCounters(pid); ok {
				out.perPID[procKey{pid, t.UnixNano()}] = c
				created[pid] = t
			}
		}
		t, ok := created[pid]
		return t, ok
	}
	// processForest calls createdAt for every PID it returns, so every returned PID whose
	// counters are readable is also measured.
	tree := processForest(entries, roots, createdAt)
	if withTerminable {
		out.terminable = selectTerminable(tree, roots, createdAt, notBefore)
	}
	return out
}

// serviceMonitor attributes work done by a Windows service's process tree to the supervised
// install.
//
// Some installers only hand their work to a service that runs outside the Job Object: msiexec
// hands the transaction to the msiserver service, and wusa hands servicing to TrustedInstaller
// and TiWorker. The CPU and I/O of the whole service tree, including the service process
// itself, count as progress of the supervised install.
//
// The accounting cannot tell which install a service is working for. Activity for an
// unrelated install, such as an SCCM or Windows Update transaction running at the same time,
// is also credited as progress. This can only delay a watchdog termination, never cause one.
type serviceMonitor struct {
	service    string
	extraRoots []string
	// gate, if non-nil, reports whether the service is currently working for an install. While
	// it returns false nothing is sampled and the baseline is reset.
	gate func() bool
	// gateOpen and gateOpenedAt record whether the gate was open at the previous sample and
	// when it was last seen to open.
	gateOpen     bool
	gateOpenedAt time.Time
	// canTerminate enables the computation of terminable processes. Monitors whose processes
	// are never terminated leave it unset.
	canTerminate bool
	start        time.Time
	agg          pidCounters
	cum          counters
	tracker      *progressTracker
	// terminable lists descendants created after start, as of the latest sample.
	terminable []uint32
}

// newServiceMonitor returns a monitor for service that starts attributing work at start.
func newServiceMonitor(service string, extraRoots []string, gate func() bool, canTerminate bool, opts Options, start time.Time) *serviceMonitor {
	return &serviceMonitor{
		service:      service,
		extraRoots:   extraRoots,
		gate:         gate,
		canTerminate: canTerminate,
		start:        start,
		tracker:      newProgressTracker(opts.progressWindow, opts.minCPUDelta, opts.minIODelta, start),
	}
}

// newMSIMonitor returns a monitor for the Windows Installer service, gated on _MSIExecute. Its
// descendants created for the install may be terminated after an abort.
func newMSIMonitor(opts Options, start time.Time) *serviceMonitor {
	return newServiceMonitor(msiServiceName, nil, msiMutexHeld, true, opts, start)
}

// newTrustedInstallerMonitor returns a monitor for the servicing stack. Its processes are never
// terminated. It has no gate, so any servicing activity on the machine counts as progress.
func newTrustedInstallerMonitor(opts Options, start time.Time) *serviceMonitor {
	return newServiceMonitor(trustedInstallerServiceName, []string{tiWorkerImage}, nil, false, opts, start)
}

// sample returns the cumulative service-side counters attributed to the install so far.
func (m *serviceMonitor) sample(now time.Time) counters {
	m.accumulate(now)
	m.tracker.observe(progressSample{at: now, cpu: m.cum.cpu, io: m.cum.io}, nil)
	return m.cum
}

// accumulate adds the service tree's counter increases since the previous call to m.cum.
func (m *serviceMonitor) accumulate(now time.Time) {
	if m.gate != nil {
		if !m.gate() {
			// A new transaction gets a fresh baseline.
			m.gateOpen = false
			m.terminable = nil
			m.agg.reset()
			return
		}
		if !m.gateOpen {
			m.gateOpen = true
			m.gateOpenedAt = now
		}
	}
	s := sampleServiceTree(m.service, m.extraRoots, m.canTerminate, m.start.Add(-creationTimeSlack))
	m.terminable = s.terminable
	dCPU, dIO := m.agg.update(s.perPID)
	m.cum.cpu += dCPU
	m.cum.io += dIO
}

// observe samples the service tree and returns the observation used by the post-abort wait.
func (m *serviceMonitor) observe(now time.Time) serviceObservation {
	m.sample(now)
	return serviceObservation{
		lastProgress: m.tracker.lastProgress,
		lastReason:   m.tracker.lastReason,
		gateOpened:   m.gateOpenedAt,
		terminable:   m.terminable,
	}
}

// afterMSIAbort runs after a job that ran msiexec was terminated; see waitForMSITransaction.
func (m *serviceMonitor) afterMSIAbort(opts Options) {
	waitForMSITransaction(opts, msiWaitEnv{
		now:       time.Now,
		sleep:     time.Sleep,
		mutexHeld: msiMutexHeld,
		observe:   m.observe,
		terminate: terminateProcesses,
	})
}

// terminateProcesses terminates each process in pids, ignoring processes that already exited.
func terminateProcesses(pids []uint32) {
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
		if err != nil {
			continue
		}
		if err := windows.TerminateProcess(h, 1); err != nil {
			logger.Warningf("Failed to terminate service-side Windows Installer process %d: %v", pid, err)
		}
		windows.CloseHandle(h)
	}
}
