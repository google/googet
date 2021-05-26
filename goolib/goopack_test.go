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
package goolib

import (
	"archive/tar"
	"bytes"
	"flag"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google3/third_party/golang/googet/oswrap/oswrap"
)

var (
	outputDir = flag.String("output_dir", "", "where to put the built package")
)

func TestPathMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		result        bool
	}{
		{"/path**.file", "/path/to.file", true},
		{"/path[a-z]", "/pathb", false},
		{"/path[a-z]", "/path[a-z]", true},
		{"path/*/file", "path/to/file", true},
		{"path/*/file", "path/to/the/file", false},
		{"path/**/file", "path/to/the/file", true},
		{"^$[a(-z])%{}}\\{{\\", "^$[a(-z])%{}}\\{{\\", true},
	}

	for _, test := range tests {
		res, err := pathMatch(test.pattern, test.path)
		if err != nil {
			t.Fatalf("match %q %q: %v", test.pattern, test.path, err)
		}
		if res != test.result {
			t.Fatalf("match %q %q: expected %v got %v", test.pattern, test.path, test.result, res)
		}
	}
}

func TestMergeWalks(t *testing.T) {
	before := []pathWalk{
		{[][]string{{"path", "to", "file"}}, -1},
		// Foo/bar/baz cases cover that the outer and inner loops of the walk
		// elimination need to be travel in opposite directions.
		{[][]string{{"foo", "bar", "*.txt"}}, 2},
		{[][]string{{"foo", "baz", "*"}}, 2},
		// Ensure coverage of element removal from both end and middle.
		{[][]string{{"path", "to", "other", "file"}}, -1},
		{[][]string{{"foo", "*"}}, 1},
	}
	expected := []pathWalk{
		{[][]string{{"path", "to", "file"}}, -1},
		{
			[][]string{
				{"foo", "bar", "*.txt"},
				{"foo", "baz", "*"},
				{"foo", "*"},
			}, 1,
		},
		{[][]string{{"path", "to", "other", "file"}}, -1},
	}
	after := mergeWalks(before)

	if !reflect.DeepEqual(after, expected) {
		t.Fatalf("mergeWalks: \nexp'd  %v \nactual %v", expected, after)
	}
}

func TestCreatePackage(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)
	sf1 := filepath.FromSlash(path.Join(tempDir, "somefile1.file"))
	f1, err := oswrap.Create(sf1)
	if err != nil {
		t.Fatalf("error creating a test file: %v", err)
	}
	f1.Close()

	sf2 := filepath.FromSlash(path.Join(tempDir, "somefile2.file"))
	f2, err := oswrap.Create(sf2)
	if err != nil {
		t.Fatalf("error creating a test file: %v", err)
	}
	f2.Close()

	outDir := *outputDir
	if outDir == "" {
		var err error
		outDir, err = os.Getwd()
		if err != nil {
			t.Fatal("error creating output directory", err)
		}
	}

	gs := &GooSpec{
		Sources: []PkgSources{
			{
				Include: []string{"**"},
				Root:    "some/place",
			}},
		PackageSpec: &PkgSpec{
			Name:         "somepkg",
			Version:      "0.0.0@1",
			Arch:         "noarch",
			ReleaseNotes: []string{"0.0.0@1 - initial release"},
			Description:  "some test package",
			Author:       "some author",
			Owners:       "some owner",
			Install: ExecFile{
				Path: "someinstallfile.ps1",
			},
		}
	}
	/*gs := GooSpec{}
	gs.Build.Windows = ("\"c:/Windows/System32/somecmd.exe\"")
	gs.Build.WindowsArgs = []string{"\"/c\"]", "\"go build -ldflags=-X=main.version={{\"0.0.0\"}} -o some.exe\""}
	gs.Build.Linux = ("\"/bin/bash\"")
	gs.Build.LinuxArgs = []string{"\"-c\"","\"GOOS=windows go build -ldflags='-X main.version={{\"0.0.0\"}}'\""}

	gs.Sources.
		//[]PkgSources
	gs.PackageSpec.Name = "somepkg"
	gs.PackageSpec.Version = "0.0.0"
	gs.PackageSpec.Arch = "x86_64"
	gs.PackageSpec.ReleaseNotes = []string{"0.0.0", "initial realease"}
	gs.PackageSpec.Description = "some test package"
	gs.PackageSpec.License = "NA"
	gs.PackageSpec.Authors = "njaiswal"
	gs.PackageSpec.Owners = "njaiswal"*/

	//("{\n\"windows\":  \"c:/Windows/System32/cmd.exe\",\n\"windowsArgs\":[\"/c\"], \"go build -ldflags=-X=main.version={{\"0.0.0\"}} -o googet.exe\"],
//\n\"linux\": \"/bin/bash\", \n\"linuxArgs\": [\"-c\",\"GOOS=windows go build -ldflags='-X main.version={{\"0.0.0\"}}'\"]\n}") }
	//gs.Sources =  ("[{\n\"include\": [\n\"somefile1.file\",\n\"somefile2.file\n\"]}\n]")
	//gs.PackageSpec = ("{{\"$version := \"0.0.0\" -}}\n{\"name\":\"googet\",\n}\"version\":{{$version}}\",\n\"arch\": \"x86_64\",\n\"authors\":\"nanditajaiswal@google.com\",\n\"license\": \"N/A\",\n\"description\": \"Test Package Manager\",\n\"releaseNotes\": [\"2.17.2 - Add Source field in GooGet Spec\",],\n\"sources\": [{\"include\"[\"somefile1.file\",\"somefile2.file\"]}],")

	if _, err := CreatePackage(&gs, tempDir, outDir); err != nil {
		t.Fatal("error creating package", err)
	}
}

func TestMapFiles(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)
	wf1 := filepath.FromSlash(path.Join(tempDir, "globme.file"))
	f, err := oswrap.Create(wf1)
	if err != nil {
		t.Fatalf("error creating test file: %v", err)
	}
	f.Close()
	f, err = oswrap.Create(path.Join(tempDir, "notme.file"))
	if err != nil {
		t.Fatalf("error creating test file: %v", err)
	}
	f.Close()
	wd := path.Join(tempDir, "globdir")
	if err := oswrap.Mkdir(wd, 0755); err != nil {
		t.Fatalf("error creating test directory: %v", err)
	}
	wf2 := filepath.FromSlash(path.Join(wd, "globmetoo.file"))
	f, err = oswrap.Create(wf2)
	if err != nil {
		t.Fatalf("error creating test file: %v", err)
	}
	f.Close()
	f, err = oswrap.Create(path.Join(tempDir, "notmeeither.file"))
	if err != nil {
		t.Fatalf("error creating test file: %v", err)
	}
	f.Close()

	ps := []PkgSources{
		{
			Include: []string{"**"},
			Exclude: []string{"notme*"},
			Target:  "foo",
			Root:    tempDir,
		},
	}
	fm, err := mapFiles(ps)
	if err != nil {
		t.Fatalf("error getting file map: %v", err)
	}
	em := fileMap{"foo": []string{wf1}, strings.Join([]string{"foo", "globdir"}, string(filepath.Separator)): []string{wf2}}
	if !reflect.DeepEqual(fm, em) {
		t.Errorf("did not get expected package map: got %v, want %v", fm, em)
	}
}

func TestWriteFiles(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Errorf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)
	wf := path.Join(tempDir, "test.pkg")
	f, err := oswrap.Create(wf)
	if err != nil {
		t.Errorf("error creating test package: %v", err)
	}
	f.Close()
	fm := fileMap{"foo": []string{wf}}
	ef := path.Join("foo", path.Base(wf))

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := writeFiles(tw, fm); err != nil {
		t.Errorf("error writing files to zip: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Errorf("error closing zip writer: %v", err)
	}
	tr := tar.NewReader(buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Error(err)
	}
	if hdr.Name != ef {
		t.Errorf("zip contains unexpected file: expect %q got %q", ef, f.Name())
	}
}

