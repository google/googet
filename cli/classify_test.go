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
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"os"
	"sort"
	"testing"

	"github.com/google/subcommands"
)

// stubCmd is a command named name with a -dry_run bool flag and a -sources
// string flag. It records whether its Execute was reached. It doesn't
// implement AllowUnelevated.
type stubCmd struct {
	name    string
	dryRun  bool
	sources string
	reached bool
}

func (c *stubCmd) Name() string   { return c.name }
func (*stubCmd) Synopsis() string { return "stub command" }
func (*stubCmd) Usage() string    { return "stub [flags] <name>...\n" }
func (c *stubCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&c.dryRun, "dry_run", false, "dry run")
	f.StringVar(&c.sources, "sources", "", "sources")
}
func (c *stubCmd) Execute(context.Context, *flag.FlagSet, ...any) subcommands.ExitStatus {
	c.reached = true
	return subcommands.ExitSuccess
}

// allowStubCmd is stubCmd that allows unelevated dry runs.
type allowStubCmd struct{ stubCmd }

func (c *allowStubCmd) AllowUnelevated() bool { return c.dryRun }

// classifyCommander returns a commander with:
//   - "allowed", which allows unelevated dry runs;
//   - "plain", which doesn't implement AllowUnelevated;
//   - "dup", registered twice, both allowing unelevated dry runs;
//   - "aliased", an alias of a command that allows unelevated dry runs.
func classifyCommander() *subcommands.Commander {
	cmdr := subcommands.NewCommander(flag.NewFlagSet("googet", flag.ContinueOnError), "googet")
	cmdr.Output, cmdr.Error = io.Discard, io.Discard
	cmdr.Register(&allowStubCmd{stubCmd{name: "allowed"}}, "package management")
	cmdr.Register(&stubCmd{name: "plain"}, "")
	cmdr.Register(&allowStubCmd{stubCmd{name: "dup"}}, "package management")
	cmdr.Register(&allowStubCmd{stubCmd{name: "dup"}}, "other")
	cmdr.Register(subcommands.Alias("aliased", &allowStubCmd{stubCmd{name: "orig"}}), "")
	return cmdr
}

func TestClassifyTable(t *testing.T) {
	cmdr := classifyCommander()
	for _, tc := range []struct {
		args []string
		want Class
	}{
		{nil, UsageOnly},
		{[]string{}, UsageOnly},
		{[]string{"allowed"}, Privileged},
		{[]string{"allowed", "pkg"}, Privileged},
		{[]string{"allowed", "-dry_run", "pkg"}, UnelevatedRun},
		{[]string{"allowed", "-dry_run"}, UnelevatedRun},
		{[]string{"allowed", "-dry_run=true", "pkg"}, UnelevatedRun},
		{[]string{"allowed", "-dry_run=false", "pkg"}, Privileged},
		{[]string{"allowed", "--dry_run", "pkg"}, UnelevatedRun},
		{[]string{"allowed", "--dry_run=false", "pkg"}, Privileged},
		{[]string{"allowed", "-dry_run", "-dry_run=false", "pkg"}, Privileged},
		{[]string{"allowed", "-dry_run=false", "-dry_run", "pkg"}, UnelevatedRun},
		{[]string{"allowed", "-dry_run", "-dry_run", "pkg"}, UnelevatedRun},
		{[]string{"allowed", "--", "-dry_run"}, Privileged},
		{[]string{"allowed", "-dry_run", "--", "-pkg", "-x"}, UnelevatedRun},
		{[]string{"allowed", "--"}, Privileged},
		{[]string{"allowed", "-sources", "-dry_run", "pkg"}, Privileged}, // -dry_run is the value of -sources.
		{[]string{"allowed", "pkg", "-dry_run"}, UsageOnly},
		{[]string{"allowed", "-h"}, UsageOnly},
		{[]string{"allowed", "-help"}, UsageOnly},
		{[]string{"allowed", "--help"}, UsageOnly},
		{[]string{"allowed", "-dry_run", "-h"}, UsageOnly},
		{[]string{"allowed", "-bogus"}, UsageOnly},
		{[]string{"allowed", "-dry_run", "-bogus", "pkg"}, UsageOnly},
		{[]string{"allowed", "-dryrun", "pkg"}, UsageOnly},
		{[]string{"allowed", "-dry_run=maybe", "pkg"}, UsageOnly},
		{[]string{"allowed", "-sources"}, UsageOnly},
		{[]string{"nosuchcmd"}, UsageOnly},
		{[]string{"nosuchcmd", "-dry_run"}, UsageOnly},
		{[]string{"ALLOWED", "-dry_run", "pkg"}, UsageOnly},
		{[]string{"Allowed", "-dry_run", "pkg"}, UsageOnly},
		{[]string{"", "-dry_run"}, UsageOnly},
		{[]string{"plain", "-dry_run", "pkg"}, Privileged},
		{[]string{"plain", "-h"}, UsageOnly},
		{[]string{"dup", "-dry_run", "pkg"}, Privileged},
		{[]string{"dup", "-h"}, UsageOnly},
		{[]string{"aliased", "-dry_run", "pkg"}, Privileged},
		{[]string{"orig", "-dry_run", "pkg"}, UsageOnly}, // Only the alias is registered.
	} {
		t.Run(fmt.Sprintf("%q", tc.args), func(t *testing.T) {
			if got, _, _ := Classify(cmdr, tc.args); got != tc.want {
				t.Errorf("Classify(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestClassifyReturnsCommand checks the command and the CheckArgs error that
// Classify returns with the class.
func TestClassifyReturnsCommand(t *testing.T) {
	cmdr := classifyCommander()
	for _, tc := range []struct {
		args     []string
		wantName string // The returned command's name, or "" for nil.
		wantErr  bool
	}{
		{args: nil},
		{args: []string{"nosuchcmd"}},
		{args: []string{"allowed", "-dry_run", "pkg"}, wantName: "allowed"},
		{args: []string{"allowed", "pkg"}, wantName: "allowed"},
		{args: []string{"allowed", "-h"}, wantName: "allowed"},
		{args: []string{"allowed", "-bogus"}, wantName: "allowed"},
		{args: []string{"allowed", "pkg", "-dry_run"}, wantName: "allowed", wantErr: true},
		{args: []string{"dup", "-dry_run", "pkg"}, wantName: "dup"},
	} {
		t.Run(fmt.Sprintf("%q", tc.args), func(t *testing.T) {
			_, cmd, err := Classify(cmdr, tc.args)
			name := ""
			if cmd != nil {
				name = cmd.Name()
			}
			if name != tc.wantName {
				t.Errorf("Classify(%q) returned command %q, want %q", tc.args, name, tc.wantName)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("Classify(%q) error = %v, want error: %v", tc.args, err, tc.wantErr)
			}
		})
	}
}

// TestClassifyNoStaleFlags checks that a flag parsed by an earlier Classify
// call doesn't leak into a later one through the registered command's fields.
func TestClassifyNoStaleFlags(t *testing.T) {
	cmdr := classifyCommander()
	if got, _, _ := Classify(cmdr, []string{"allowed", "-dry_run", "pkg"}); got != UnelevatedRun {
		t.Fatalf("Classify(allowed -dry_run pkg) = %v, want UnelevatedRun", got)
	}
	if got, _, _ := Classify(cmdr, []string{"allowed", "pkg"}); got != Privileged {
		t.Errorf("Classify(allowed pkg) after a dry run = %v, want Privileged", got)
	}
}

// TestClassifyMatchesExecute checks, for the same command lines, that
// Classify returns UsageOnly exactly when subcommands.Commander.Execute
// doesn't reach the command's Execute, or when CheckArgs rejects the
// arguments. This pins the parse that Classify copies from subcommands.
func TestClassifyMatchesExecute(t *testing.T) {
	names := []string{"stub", "STUB", "nosuchcmd", ""}
	argSets := [][]string{
		nil,
		{"pkg"},
		{"-dry_run", "pkg"},
		{"-dry_run=false", "pkg"},
		{"-dry_run=maybe", "pkg"},
		{"--dry_run", "pkg"},
		{"-dry_run", "-dry_run=false"},
		{"-h"},
		{"-help"},
		{"--help"},
		{"-bogus"},
		{"-dry_run", "-bogus", "pkg"},
		{"-sources"},
		{"-sources", "--", "pkg"},
		{"-sources", "--", "pkg", "-x"},
		{"pkg", "-dry_run"},
		{"--", "-x"},
		{"--"},
		{"-"},
		{"-", "-"},
		{"pkg", "--"},
	}
	for _, name := range names {
		for _, rest := range argSets {
			args := append([]string{name}, rest...)
			t.Run(fmt.Sprintf("%q", args), func(t *testing.T) {
				top := flag.NewFlagSet("googet", flag.ContinueOnError)
				top.SetOutput(io.Discard)
				cmdr := subcommands.NewCommander(top, "googet")
				cmdr.Output, cmdr.Error = io.Discard, io.Discard
				stub := &allowStubCmd{stubCmd{name: "stub"}}
				cmdr.Register(stub, "")
				if err := top.Parse(args); err != nil {
					t.Fatalf("top-level Parse(%q): %v", args, err)
				}
				class, _, _ := Classify(cmdr, top.Args())
				silenceStderr(t, func() { cmdr.Execute(context.Background()) })
				checkErr := CheckArgs(stub, rest)
				wantUsage := !stub.reached || (name == "stub" && checkErr != nil)
				if (class == UsageOnly) != wantUsage {
					t.Errorf("Classify(%q) = %v; Execute reached the command: %v, CheckArgs: %v", args, class, stub.reached, checkErr)
				}
			})
		}
	}
}

// silenceStderr runs f with os.Stderr redirected away, because subcommands
// prints flag parse errors there.
func silenceStderr(t *testing.T, f func()) {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	old := os.Stderr
	os.Stderr = null
	defer func() { os.Stderr = old }()
	f()
}

func TestRefuseUnlessAllowed(t *testing.T) {
	for _, tc := range []struct {
		privileged, allow, want bool
	}{
		{privileged: false, allow: false, want: true},
		{privileged: false, allow: true, want: false},
		{privileged: true, allow: false, want: false},
		{privileged: true, allow: true, want: false},
	} {
		t.Run(fmt.Sprintf("privileged=%v,allow=%v", tc.privileged, tc.allow), func(t *testing.T) {
			ctx := context.Background()
			if tc.privileged {
				ctx = WithPrivileged(ctx)
			}
			cmd := &allowStubCmd{stubCmd{name: "stub", dryRun: tc.allow}}
			if got := RefuseUnlessAllowed(ctx, cmd); got != tc.want {
				t.Errorf("RefuseUnlessAllowed(privileged=%v, AllowUnelevated()=%v) = %v, want %v", tc.privileged, tc.allow, got, tc.want)
			}
		})
	}
}

// TestRefuseUnlessAllowedFirst checks that in every command under cli that
// implements AllowUnelevated, the first statement of Execute is
//
//	if cli.RefuseUnlessAllowed(<ctx>, <receiver>) { ...; return ... }
//
// so the command refuses work its AllowUnelevated doesn't allow before it
// touches anything.
func TestRefuseUnlessAllowedFirst(t *testing.T) {
	files, fset := parseGuardFiles(t)
	allow := map[string]bool{}          // "dir.Type" of each implementer.
	execs := map[string]*ast.FuncDecl{} // "dir.Type" to its Execute method.
	for _, gf := range files {
		for _, d := range gf.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			name := gf.dir() + "." + recvTypeName(fd.Recv.List[0].Type)
			switch fd.Name.Name {
			case "AllowUnelevated":
				allow[name] = true
			case "Execute":
				execs[name] = fd
			}
		}
	}
	var checked []string
	for name := range allow {
		checked = append(checked, name)
		fd := execs[name]
		if fd == nil {
			t.Errorf("%s implements AllowUnelevated but has no Execute method", name)
			continue
		}
		if msg := refusalFirst(fd); msg != "" {
			t.Errorf("%s.Execute at %s: %s", name, fset.Position(fd.Pos()), msg)
		}
	}
	sort.Strings(checked)
	t.Logf("checked %q", checked)
	// installed, listrepos and remove implement AllowUnelevated, so finding
	// fewer means the scan is broken.
	if len(checked) < 3 {
		t.Errorf("found %d AllowUnelevated implementers (%q), want at least 3", len(checked), checked)
	}
}

// recvTypeName returns the type name of a method receiver, without "*".
func recvTypeName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// refusalFirst returns why fd's first statement isn't
// "if cli.RefuseUnlessAllowed(<ctx param>, <receiver>) { ... return }", or ""
// if it is.
func refusalFirst(fd *ast.FuncDecl) string {
	if fd.Body == nil || len(fd.Body.List) == 0 {
		return "empty body"
	}
	ifs, ok := fd.Body.List[0].(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil {
		return "first statement isn't a plain if statement"
	}
	call, ok := ifs.Cond.(*ast.CallExpr)
	if !ok {
		return "if condition isn't a call"
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "RefuseUnlessAllowed" {
		return "if condition doesn't call cli.RefuseUnlessAllowed"
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "cli" {
		return "if condition doesn't call cli.RefuseUnlessAllowed"
	}
	if len(call.Args) != 2 {
		return "cli.RefuseUnlessAllowed doesn't get two arguments"
	}
	params := fd.Type.Params.List
	if len(params) == 0 || len(params[0].Names) == 0 {
		return "Execute has no named context parameter"
	}
	if id, ok := call.Args[0].(*ast.Ident); !ok || id.Name != params[0].Names[0].Name {
		return "cli.RefuseUnlessAllowed doesn't get Execute's context"
	}
	recv := fd.Recv.List[0]
	if len(recv.Names) == 0 {
		return "Execute has no named receiver"
	}
	if id, ok := call.Args[1].(*ast.Ident); !ok || id.Name != recv.Names[0].Name {
		return "cli.RefuseUnlessAllowed doesn't get the receiver"
	}
	body := ifs.Body.List
	if len(body) == 0 {
		return "the refusal doesn't return"
	}
	if _, ok := body[len(body)-1].(*ast.ReturnStmt); !ok {
		return "the refusal doesn't end with a return"
	}
	return ""
}

// TestRefusalFirst checks refusalFirst on small sources, so that
// TestRefuseUnlessAllowedFirst can't pass by accepting an Execute whose first
// statement isn't the refusal.
func TestRefusalFirst(t *testing.T) {
	const header = `package x

import (
	"context"

	"github.com/google/googet/v2/cli"
)

type cmd struct{}

`
	for _, tc := range []struct {
		desc    string
		execute string
		wantMsg string
	}{
		{
			desc: "valid",
			execute: `func (c *cmd) Execute(ctx context.Context) int {
	if cli.RefuseUnlessAllowed(ctx, c) {
		println("refused")
		return 1
	}
	return 0
}`,
		},
		{
			desc: "refusal not first",
			execute: `func (c *cmd) Execute(ctx context.Context) int {
	println("working")
	if cli.RefuseUnlessAllowed(ctx, c) {
		return 1
	}
	return 0
}`,
			wantMsg: "first statement isn't a plain if statement",
		},
		{
			desc: "wrong function",
			execute: `func (c *cmd) Execute(ctx context.Context) int {
	if cli.IsPrivileged(ctx) {
		return 1
	}
	return 0
}`,
			wantMsg: "if condition doesn't call cli.RefuseUnlessAllowed",
		},
		{
			desc: "wrong context",
			execute: `func (c *cmd) Execute(ctx context.Context) int {
	if cli.RefuseUnlessAllowed(context.Background(), c) {
		return 1
	}
	return 0
}`,
			wantMsg: "cli.RefuseUnlessAllowed doesn't get Execute's context",
		},
		{
			desc: "receiver not passed",
			execute: `func (c *cmd) Execute(ctx context.Context) int {
	if cli.RefuseUnlessAllowed(ctx, &cmd{}) {
		return 1
	}
	return 0
}`,
			wantMsg: "cli.RefuseUnlessAllowed doesn't get the receiver",
		},
		{
			desc: "missing return",
			execute: `func (c *cmd) Execute(ctx context.Context) int {
	if cli.RefuseUnlessAllowed(ctx, c) {
		println("refused")
	}
	return 0
}`,
			wantMsg: "the refusal doesn't end with a return",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			gf, err := newGuardFile(token.NewFileSet(), "x/x.go", header+tc.execute+"\n")
			if err != nil {
				t.Fatal(err)
			}
			var fd *ast.FuncDecl
			for _, d := range gf.file.Decls {
				if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "Execute" {
					fd = f
				}
			}
			if fd == nil {
				t.Fatal("source has no Execute method")
			}
			if got := refusalFirst(fd); got != tc.wantMsg {
				t.Errorf("refusalFirst(%s) = %q, want %q", tc.desc, got, tc.wantMsg)
			}
		})
	}
}
