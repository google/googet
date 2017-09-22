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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
)

type build struct {
	Windows, Linux         string
	WindowsArgs, LinuxArgs []string
}

// PkgSources is a list of includes, excludes and their target in the package.
type PkgSources struct {
	Include, Exclude []string
	Target, Root     string
}

// GooSpec is the build specification for a package.
type GooSpec struct {
	Build       build
	Sources     []PkgSources
	PackageSpec *PkgSpec
}

// RepoSpec is the repository specfication of a package.
type RepoSpec struct {
	Checksum, Source string
	PackageSpec      *PkgSpec
}

// Marshal returns the formatted RepoSpec.
func (rs *RepoSpec) Marshal() ([]byte, error) {
	return json.MarshalIndent(rs, "", "  ")
}

const (
	pkgSpecSuffix   = ".pkgspec"
	maxTagKeyLen    = 127
	maxTagValueSize = 1024 * 10 // 10k
)

var validArch = []string{"noarch", "x86_64", "x86_32", "arm"}

// PkgSpec is the internal package specification.
type PkgSpec struct {
	Name            string
	Version         string
	Arch            string
	ReleaseNotes    []string          `json:",omitempty"`
	Description     string            `json:",omitempty"`
	License         string            `json:",omitempty"`
	Authors         string            `json:",omitempty"`
	Owners          string            `json:",omitempty"`
	Tags            map[string][]byte `json:",omitempty"`
	PkgDependencies map[string]string `json:",omitempty"`
	Install         ExecFile
	Uninstall       ExecFile
	Files           map[string]string `json:",omitempty"`
}

// ExecFile contains info involved in running a script or binary file.
type ExecFile struct {
	Path      string   `json:",omitempty"`
	Args      []string `json:",omitempty"`
	ExitCodes []int    `json:",omitempty"`
}

// Version contains the semver version as well as the GsVer.
// Semver is semantic versioning version.
// GsVer is a GooSpec version number (usually version of installer).
type Version struct {
	Semver semver.Version
	GsVer  int
}

// Ver returns the goospec version.
func (gs GooSpec) Ver() (Version, error) {
	return ParseVersion(gs.PackageSpec.Version)
}

func (gs GooSpec) verify() error {
	return gs.PackageSpec.verify()
}

// Compare compares string versions of packages v1 to v2:
// -1 == v1 is less than v2
// 0 == v1 is equal to v2
// 1 == v1 is greater than v2
func Compare(v1, v2 string) (int, error) {
	pv1, err := ParseVersion(v1)
	if err != nil {
		return 0, err
	}
	pv2, err := ParseVersion(v2)
	if err != nil {
		return 0, err
	}
	var c int
	if c = pv1.Semver.Compare(pv2.Semver); c == 0 {
		if pv1.GsVer > pv2.GsVer {
			return 1, nil
		}
		if pv1.GsVer < pv2.GsVer {
			return -1, nil
		}
		return 0, nil
	}
	return c, nil
}

func fixVer(ver string) string {
	suffix := ""
	// Patch number can contain PreRelease/Build meta data suffix.
	if i := strings.IndexAny(ver, "+-"); i != -1 {
		suffix = ver[i:]
		ver = ver[:i]
	}
	out := []string{"0", "0", "0"}
	nums := strings.SplitN(ver, ".", 3)
	offset := len(out) - len(nums)
	for i, str := range nums {
		trimmed := strings.TrimLeft(str, "0")
		if trimmed == "" {
			trimmed = "0"
		}
		out[i+offset] = trimmed
	}
	return strings.Join(out, ".") + suffix
}

// ParseVersion parses the string version into goospec.Version. ParseVersion
// attempts to fix non-compliant Semver strings by removing leading zeros from
// components, and replacing any missing components with zero values after
// using existing components for the least significant components first (i.e.
// "1" will become "0.0.1", not "1.0.0").
func ParseVersion(ver string) (Version, error) {
	v := strings.SplitN(ver, "@", 2)
	v[0] = fixVer(v[0])

	sv, err := semver.Parse(v[0])
	if err != nil {
		return Version{}, err
	}
	version := Version{Semver: sv}
	if len(v) == 2 {
		gv, err := strconv.ParseInt(v[1], 10, 32)
		if err != nil {
			return version, err
		}
		version = Version{
			Semver: sv,
			GsVer:  int(gv),
		}
	} else {
		version = Version{Semver: sv}
	}
	return version, nil
}

// Versions contains a list of goospec string versions.
type Versions []string

func (s Versions) Len() int {
	return len(s)
}

func (s Versions) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s Versions) Less(i, j int) bool {
	c, err := Compare(s[i], s[j])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Compare failed, %q or %s are not a proper version", s[i], s[j])
	}
	return c == -1
}

// SortVersions sorts a list of goospec string versions.
func SortVersions(versions []string) []string {
	var vl []string
	for i, v := range versions {
		if _, err := ParseVersion(v); err != nil {
			fmt.Fprintf(os.Stderr, "Removing %q from list: %v", v, err)
			continue
		}
		vl = append(vl, versions[i])
	}

	sort.Sort(Versions(vl))
	return vl
}

func unmarshalGooSpec(c []byte) (GooSpec, error) {
	var gs GooSpec
	//var temp pkgSpec
	if err := json.Unmarshal(c, &gs.PackageSpec); err != nil {
		return gs, err
	}
	if err := json.Unmarshal(c, &gs); err != nil {
		return gs, err
	}
	return gs, nil
}

// ReadGooSpec unmarshalls and verifies a goospec file into the GooSpec struct.
func ReadGooSpec(cf string) (GooSpec, error) {
	c, err := ioutil.ReadFile(cf)
	if err != nil {
		return GooSpec{}, err
	}
	gs, err := unmarshalGooSpec(c)
	if err != nil {
		return gs, err
	}
	if err = gs.verify(); err != nil {
		return gs, err
	}
	return gs, err
}

// WritePackageSpec takes a PkgSpec and writes it as a JSON file using
// the provided tar writer.
func WritePackageSpec(tw *tar.Writer, spec *PkgSpec) error {
	buf := &bytes.Buffer{}

	c, err := MarshalPackageSpec(spec)
	if err != nil {
		return err
	}
	buf.Write(c)

	fh := &tar.Header{
		Name:    spec.Name + pkgSpecSuffix,
		Size:    int64(buf.Len()),
		ModTime: time.Now(),
		Mode:    0644,
	}

	if err := tw.WriteHeader(fh); err != nil {
		return err
	}
	if _, err := tw.Write(buf.Bytes()); err != nil {
		return err
	}
	return nil
}

// ReadPackageSpec reads a PkgSpec from the given reader, which is
// expected to contain an uncompressed tar archive.
func ReadPackageSpec(r io.Reader) (*PkgSpec, error) {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, err
		}
		if filepath.Ext(header.Name) != pkgSpecSuffix {
			continue
		}

		data, err := ioutil.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		return UnmarshalPackageSpec(data)
	}
}

func (spec *PkgSpec) verify() error {
	if spec.Name == "" {
		return errors.New("no name defined in package spec")
	}
	if !ContainsString(spec.Arch, validArch) {
		return fmt.Errorf("invalid architecture: %q", spec.Arch)
	}
	if spec.Version == "" {
		return errors.New("Version string empty")
	}
	if _, err := ParseVersion(spec.Version); err != nil {
		return fmt.Errorf("can't parse %q: %v", spec.Version, err)
	}
	if len(spec.Tags) > 10 {
		return errors.New("too many tags")
	}
	for k, v := range spec.Tags {
		if len(k) > maxTagKeyLen {
			return errors.New("tag key too large")
		}
		if len(v) > maxTagValueSize {
			return fmt.Errorf("tag %q too large", k)
		}
	}
	for k, v := range spec.PkgDependencies {
		if _, err := ParseVersion(v); err != nil {
			return fmt.Errorf("can't parse version %q for dependancy %q: %v", v, k, err)
		}
	}
	for src := range spec.Files {
		if filepath.IsAbs(src) {
			return fmt.Errorf("%q is an absolute path, expected relative", src)
		}
	}
	return nil
}

// MarshalPackageSpec encodes the given PkgSpec.
func MarshalPackageSpec(spec *PkgSpec) ([]byte, error) {
	if err := spec.verify(); err != nil {
		return nil, err
	}

	return json.MarshalIndent(spec, "", "  ")
}

// UnmarshalPackageSpec parses data and returns a PkgSpec, if it finds
// one.
func UnmarshalPackageSpec(data []byte) (*PkgSpec, error) {
	var p PkgSpec
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if err := p.verify(); err != nil {
		return nil, err
	}
	return &p, nil
}
