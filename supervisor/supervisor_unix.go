//go:build !windows

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
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// clockTicksPerSecond is USER_HZ, the unit of utime and stime in /proc/<pid>/stat on Linux.
const clockTicksPerSecond = 100

// procRoot is the procfs mount point; tests may override it.
var procRoot = "/proc"

// isSession0 reports whether the current process runs in Windows Session 0, which is never true
// outside Windows.
func isSession0() bool { return false }

// procStat holds the fields of /proc/<pid>/stat used by the supervisor.
type procStat struct {
	state byte
	pgrp  int
	cpu   time.Duration
	// start is the process start time in clock ticks after boot, or 0 if the line is too short.
	start int64
}

// parseProcStat parses the contents of /proc/<pid>/stat. The command name may contain spaces
// and parentheses, so fields are located after the last ')'.
func parseProcStat(data []byte) (procStat, bool) {
	idx := bytes.LastIndexByte(data, ')')
	if idx < 0 || idx+2 >= len(data) {
		return procStat{}, false
	}
	// Fields after ')' start at field 3 (state); pgrp is field 5, utime and stime are 14 and 15
	// and starttime is 22.
	fields := strings.Fields(string(data[idx+2:]))
	if len(fields) < 13 || len(fields[0]) != 1 {
		return procStat{}, false
	}
	pgrp, err1 := strconv.Atoi(fields[2])
	utime, err2 := strconv.ParseInt(fields[11], 10, 64)
	stime, err3 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return procStat{}, false
	}
	st := procStat{
		state: fields[0][0],
		pgrp:  pgrp,
		cpu:   time.Duration(utime+stime) * time.Second / clockTicksPerSecond,
	}
	if len(fields) > 19 {
		if start, err := strconv.ParseInt(fields[19], 10, 64); err == nil {
			st.start = start
		}
	}
	return st, true
}

// parseProcIO returns read_bytes plus write_bytes from the contents of /proc/<pid>/io.
func parseProcIO(data []byte) uint64 {
	var total uint64
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "read_bytes", "write_bytes":
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err == nil {
				total += n
			}
		}
	}
	return total
}

// procStatPath returns the path of /proc/<pid>/stat under procRoot.
func procStatPath(pid int) string {
	return filepath.Join(procRoot, strconv.Itoa(pid), "stat")
}

// readProcCounters returns the counters, start time and process group of pid, or ok=false if
// unreadable. I/O counters that are unreadable, for example due to permissions, are reported as zero.
func readProcCounters(pid int) (counters, int64, int, bool) {
	data, err := os.ReadFile(procStatPath(pid))
	if err != nil {
		return counters{}, 0, 0, false
	}
	st, ok := parseProcStat(data)
	if !ok {
		return counters{}, 0, 0, false
	}
	c := counters{cpu: st.cpu}
	if ioData, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "io")); err == nil {
		c.io = parseProcIO(ioData)
	}
	return c, st.start, st.pgrp, true
}

// sampleProcessGroup returns per-process counters for every process in process group pgid. If
// /proc cannot be enumerated it falls back to the root process only.
func sampleProcessGroup(pgid, rootPID int) map[procKey]counters {
	out := make(map[procKey]counters)
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		if c, start, _, ok := readProcCounters(rootPID); ok {
			out[procKey{uint32(rootPID), start}] = c
		}
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		c, start, pgrp, ok := readProcCounters(pid)
		if ok && pgrp == pgid {
			out[procKey{uint32(pid), start}] = c
		}
	}
	if len(out) == 0 {
		if c, start, _, ok := readProcCounters(rootPID); ok {
			out[procKey{uint32(rootPID), start}] = c
		}
	}
	return out
}

// procExited reports whether pid has exited according to procfs: it is a zombie, or its entry
// is gone. It never calls wait, so the reap stays with exec.Cmd.Wait. procfsWorks must be true
// only if the process's stat file was readable earlier, so that a missing entry means it was
// reaped rather than that procfs is unavailable.
func procExited(pid int, procfsWorks bool) bool {
	if !procfsWorks {
		return false
	}
	data, err := os.ReadFile(procStatPath(pid))
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	st, ok := parseProcStat(data)
	return ok && st.state == 'Z'
}

// processGroupTree supervises a process group on Unix systems.
type processGroupTree struct {
	c           *exec.Cmd
	pid, pgid   int
	detector    windowDetectorFunc
	procfsWorks bool
	agg         pidCounters
	cum         progressSample
}

// sample returns the cumulative counters of the process group.
func (t *processGroupTree) sample(now time.Time) progressSample {
	dCPU, dIO := t.agg.update(sampleProcessGroup(t.pgid, t.pid))
	t.cum.cpu += dCPU
	t.cum.io += dIO
	t.cum.at = now
	return t.cum
}

// windows returns windows reported by the test detector; Unix has no native window detection.
func (t *processGroupTree) windows() []windowInfo {
	if t.detector == nil {
		return nil
	}
	wins, _ := t.detector([]uint32{uint32(t.pid)})
	return wins
}

// terminate sends SIGKILL to the whole process group and to the root process.
func (t *processGroupTree) terminate() {
	_ = syscall.Kill(-t.pgid, syscall.SIGKILL)
	_ = t.c.Process.Kill()
}

// afterAbort does nothing because Unix installers have no out-of-tree service side.
func (t *processGroupTree) afterAbort() {}

// rootExited reports whether the root process is a zombie or was already reaped. On systems
// without procfs it always returns false.
func (t *processGroupTree) rootExited() bool {
	return procExited(t.pid, t.procfsWorks)
}

// runSupervised executes a process in its own process group with forward progress, hard timeout
// and optional UI watchdog monitoring on Unix systems.
func runSupervised(c *exec.Cmd, opts Options, out io.Writer) error {
	devNull, err := setupStdio(c, out)
	if err != nil {
		return err
	}
	defer devNull.Close()

	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true

	if err := c.Start(); err != nil {
		return err
	}
	pid := c.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid <= 0 {
		pgid = pid
	}
	_, statErr := os.Stat(procStatPath(pid))

	waitErr := make(chan error, 1)
	go func() { waitErr <- c.Wait() }()

	tree := &processGroupTree{
		c:           c,
		pid:         pid,
		pgid:        pgid,
		detector:    opts.windowDetector,
		procfsWorks: statErr == nil,
	}
	ticker := time.NewTicker(opts.pollInterval)
	defer ticker.Stop()
	return supervise(opts, waitErr, tree, time.Now(), ticker.C)
}
