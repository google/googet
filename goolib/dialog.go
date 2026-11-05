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
// ownership and extended style is a dialog: a standard one, or an owned window
// with a modal dialog frame. This leaves out other windows, such as the hidden
// helper windows of .NET and GDI+.
func isDialogWindow(class string, owned bool, exStyle uint32) bool {
	return class == dialogClass || owned && exStyle&wsExDlgModalFrame != 0
}
