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

package cli

import (
	"context"
	"testing"
)

func TestIsPrivileged(t *testing.T) {
	ctx := context.Background()
	if IsPrivileged(ctx) {
		t.Error("IsPrivileged(Background) = true, want false")
	}
	if !IsPrivileged(WithPrivileged(ctx)) {
		t.Error("IsPrivileged(WithPrivileged(Background)) = false, want true")
	}
	type otherKey struct{}
	if IsPrivileged(context.WithValue(ctx, otherKey{}, true)) {
		t.Error("IsPrivileged with an unrelated true value = true, want false")
	}
}
