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

// This file has no build tag, unlike the Windows code that uses it, so that
// isDialogWindow is tested on every OS.

const (
	dialogClass       = "#32770" // The window class of standard dialogs.
	wsExDlgModalFrame = 0x1      // WS_EX_DLGMODALFRAME.
)

// isDialogWindow reports whether a top-level window with the given class,
// visibility, ownership and extended style is a dialog, given whether nobody
// can answer it. A dialog is any standard dialog window (class "#32770"),
// which includes some progress windows, or an owned window with
// WS_EX_DLGMODALFRAME; other progress windows, without a modal frame, and
// unowned main windows are left out. Ordinarily the window must also have
// WS_VISIBLE so hidden "#32770" helper windows (such as .NET and GDI+ helpers)
// are ignored; when unattended, a hidden dialog with WS_EX_DLGMODALFRAME also
// counts because session-0 MessageBox windows on Windows 11 omit WS_VISIBLE.
func isDialogWindow(class string, visible, owned, unattended bool, exStyle uint32) bool {
	if !visible && (!unattended || exStyle&wsExDlgModalFrame == 0) {
		return false
	}
	return class == dialogClass || owned && exStyle&wsExDlgModalFrame != 0
}
