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
	"strings"
	"time"
)

// Image names that identify installers whose real work runs in a Windows service outside the
// supervised process tree.
const (
	msiexecImage  = "msiexec.exe"
	wusaImage     = "wusa.exe"
	dismImage     = "dism.exe"
	tiWorkerImage = "tiworker.exe"
)

// imageBaseName returns the lower-cased file name of path, without directories or surrounding
// quotes.
func imageBaseName(path string) string {
	path = strings.Trim(path, `"`)
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		path = path[i+1:]
	}
	return strings.ToLower(path)
}

// isCommand reports whether path names image, with or without the .exe extension.
func isCommand(path, image string) bool {
	name := imageBaseName(path)
	return name == image || name+".exe" == image
}

// isMsiexecCommand reports whether path names the Windows Installer client, msiexec(.exe).
func isMsiexecCommand(path string) bool {
	return isCommand(path, msiexecImage)
}

// isServicingCommand reports whether path names wusa(.exe) or dism(.exe), whose work runs in the
// TrustedInstaller servicing stack.
func isServicingCommand(path string) bool {
	return isCommand(path, wusaImage) || isCommand(path, dismImage)
}

// procEntry is a minimal process table entry.
type procEntry struct {
	pid  uint32
	ppid uint32
	exe  string
}

// pidsWithImage returns the PIDs of entries whose image base name equals image, ignoring case.
func pidsWithImage(entries []procEntry, image string) []uint32 {
	var pids []uint32
	for _, e := range entries {
		if imageBaseName(e.exe) == image {
			pids = append(pids, e.pid)
		}
	}
	return pids
}

// processForest returns every root followed by all transitive descendants found in entries, in
// breadth-first order.
//
// created returns the creation time of a process, or false if it is unknown. A child is included
// only if its creation time is known and is not before its parent's, which guards against a PID
// that was reused after the original parent exited. The comparison is skipped when the parent's
// creation time is unknown.
func processForest(entries []procEntry, roots []uint32, created func(uint32) (time.Time, bool)) []uint32 {
	children := make(map[uint32][]uint32)
	for _, e := range entries {
		if e.pid != e.ppid {
			children[e.ppid] = append(children[e.ppid], e.pid)
		}
	}
	seen := make(map[uint32]bool)
	var out, queue []uint32
	for _, r := range roots {
		if r != 0 && !seen[r] {
			seen[r] = true
			out = append(out, r)
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		parentCreated, parentKnown := created(parent)
		for _, c := range children[parent] {
			if seen[c] {
				continue
			}
			childCreated, ok := created(c)
			if !ok || (parentKnown && childCreated.Before(parentCreated)) {
				continue
			}
			seen[c] = true
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// selectTerminable returns the PIDs in tree that may be terminated after an abort: processes
// created at or after notBefore, excluding every protected PID such as the service process.
func selectTerminable(tree []uint32, protected []uint32, created func(uint32) (time.Time, bool), notBefore time.Time) []uint32 {
	skip := make(map[uint32]bool, len(protected))
	for _, p := range protected {
		skip[p] = true
	}
	var out []uint32
	for _, pid := range tree {
		if skip[pid] {
			continue
		}
		if t, ok := created(pid); ok && !t.Before(notBefore) {
			out = append(out, pid)
		}
	}
	return out
}
