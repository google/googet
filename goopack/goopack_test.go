/*
Copyright 2016 Google Inc. All Rights Reserved.
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
	"flag"
	"fmt"
	"reflect"
	"testing"
)

func TestPopulateVars(t *testing.T) {
	flag.String("var:TestPopulateVars1", "", "")
	flag.String("var:TestPopulateVars2", "", "")
	flag.CommandLine.Parse([]string{"-var:TestPopulateVars1", "value", "-var:TestPopulateVars2=value"})
	want := map[string]string{"TestPopulateVars1": "value", "TestPopulateVars2": "value"}

	got := populateVars()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("want: %q, got: %q", want, got)
	}
}

func TestAddFlags(t *testing.T) {
	firstFlag := "var:first_var"
	secondFlag := "var:second_var"
	value := "value"

	flag.Bool("var:test2", false, "")
	flag.CommandLine.Parse([]string{"-var:test2"})

	args := []string{"-var:test2", "-" + firstFlag, value, fmt.Sprintf("--%s=%s", secondFlag, value), "var:not_a_flag", "also_not_a_flag"}
	before := flag.NFlag()
	addFlags(args)
	flag.CommandLine.Parse(args)
	after := flag.NFlag()

	want := before + 2
	if after != want {
		t.Errorf("number of flags after does not match expectation, want %d, got %d", want, after)
	}

	for _, fn := range []string{firstFlag, secondFlag} {
		got := flag.Lookup(fn).Value.String()
		if got != value {
			t.Errorf("flag %q value %q!=%q", fn, got, value)
		}
	}
}

