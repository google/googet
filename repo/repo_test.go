package repo

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
)

func TestRepoList(t *testing.T) {
	logger.Init("GooGet", true, false, io.Discard)
	testRepo := "https://foo.com/googet/bar"
	testHTTPRepo := "http://foo.com/googet/bar"

	for i, tc := range []struct {
		content        []byte
		want           map[string]priority.Value
		allowUnsafeURL bool
	}{
		{[]byte("\n"), nil, false},
		{[]byte("# This is just a comment"), nil, false},
		{[]byte("url: " + testRepo), map[string]priority.Value{testRepo: priority.Default}, false},
		{[]byte("\n # Comment\nurl: " + testRepo), map[string]priority.Value{testRepo: priority.Default}, false},
		{[]byte("- url: " + testRepo), map[string]priority.Value{testRepo: priority.Default}, false},
		// The HTTP repo should be dropped.
		{[]byte("- url: " + testHTTPRepo), nil, false},
		// The HTTP repo should not be dropped.
		{[]byte("- url: " + testHTTPRepo), map[string]priority.Value{testHTTPRepo: priority.Default}, true},
		{[]byte("- URL: " + testRepo), map[string]priority.Value{testRepo: priority.Default}, false},
		// The HTTP repo should be dropped.
		{[]byte("- url: " + testRepo + "\n\n- URL: " + testHTTPRepo), map[string]priority.Value{testRepo: priority.Default}, false},
		// The HTTP repo should not be dropped.
		{[]byte("- url: " + testRepo + "\n\n- URL: " + testHTTPRepo), map[string]priority.Value{testRepo: priority.Default, testHTTPRepo: 500}, true},
		{[]byte("- url: " + testRepo + "\n\n- URL: " + testRepo), map[string]priority.Value{testRepo: priority.Default}, false},
		{[]byte("- url: " + testRepo + "\n\n- url: " + testRepo), map[string]priority.Value{testRepo: priority.Default}, false},
		// Should contain oauth- prefix
		{[]byte("- url: " + testRepo + "\n  useoauth: true"), map[string]priority.Value{"oauth-" + testRepo: priority.Default}, false},
		// Should not contain oauth- prefix
		{[]byte("- url: " + testRepo + "\n  useoauth: false"), map[string]priority.Value{testRepo: priority.Default}, false},
		{[]byte("- url: " + testRepo + "\n  priority: 1200"), map[string]priority.Value{testRepo: priority.Value(1200)}, false},
		{[]byte("- url: " + testRepo + "\n  priority: default"), map[string]priority.Value{testRepo: priority.Default}, false},
		{[]byte("- url: " + testRepo + "\n  priority: canary"), map[string]priority.Value{testRepo: priority.Canary}, false},
		{[]byte("- url: " + testRepo + "\n  priority: pin"), map[string]priority.Value{testRepo: priority.Pin}, false},
		{[]byte("- url: " + testRepo + "\n  priority: rollback"), map[string]priority.Value{testRepo: priority.Rollback}, false},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			tempDir := t.TempDir()
			testFile := filepath.Join(tempDir, "test.repo")
			if err := os.WriteFile(testFile, tc.content, 0660); err != nil {
				t.Fatalf("error writing repo: %v", err)
			}
			settings.AllowUnsafeURL = tc.allowUnsafeURL
			got, err := repoList(tempDir)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("repoList unexpected diff (-want +got): %v", diff)
			}
		})
	}
}

func TestWriteOrDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []Entry
		want    string
	}{
		{
			name:    "with-no-priority-specified",
			entries: []Entry{{Name: "bar", URL: "https://foo.com/googet/bar"}},
			want: `- name: bar
  url: https://foo.com/googet/bar
  useoauth: false
`,
		},
		{
			name:    "with-default-priority",
			entries: []Entry{{Name: "bar", URL: "https://foo.com/googet/bar", Priority: priority.Default}},
			want: `- name: bar
  url: https://foo.com/googet/bar
  useoauth: false
  priority: default
`,
		},
		{
			name:    "with-rollback-priority",
			entries: []Entry{{Name: "bar", URL: "https://foo.com/googet/bar", Priority: priority.Rollback}},
			want: `- name: bar
  url: https://foo.com/googet/bar
  useoauth: false
  priority: rollback
`,
		},
		{
			name:    "with-non-standard-priority",
			entries: []Entry{{Name: "bar", URL: "https://foo.com/googet/bar", Priority: 42}},
			want: `- name: bar
  url: https://foo.com/googet/bar
  useoauth: false
  priority: 42
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp("", "test.repo")
			if err != nil {
				t.Fatalf("os.CreateTemp: %v", err)
			}
			defer func() {
				os.Remove(f.Name())
			}()
			if err := f.Close(); err != nil {
				t.Fatalf("f.Close: %v", err)
			}
			rf := File{Path: f.Name(), Entries: tc.entries}
			if err := rf.writeOrDelete(); err != nil {
				t.Fatalf("write: %v", err)
			}
			b, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatalf("os.ReadFile(%v): %v", f.Name(), err)
			}
			t.Logf("wrote repo file contents:\n%v", string(b))
			// Make the diff easier to read by splitting into lines first.
			got := strings.Split(string(b), "\n")
			want := strings.Split(tc.want, "\n")
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("write got unexpected diff (-want +got):\n%v", diff)
			}
		})
	}
}

// TestWriteOrDeleteReplace checks that rewriting a repo file leaves only the
// repo file, holding the new entries, in its directory, and that a replaced
// file keeps its mode while a new one gets 0664.
func TestWriteOrDeleteReplace(t *testing.T) {
	newEntries := []Entry{{Name: "bar", URL: "https://foo.com/googet/bar"}}
	t.Run("rewrites contents", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "test.repo")
		const old = "- name: old\n  url: https://old.example/googet\n"
		if err := os.WriteFile(path, []byte(old), 0644); err != nil {
			t.Fatal(err)
		}
		rf := File{Path: path, Entries: newEntries}
		if err := rf.writeOrDelete(); err != nil {
			t.Fatalf("writeOrDelete: %v", err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "name: bar") || strings.Contains(string(b), "name: old") {
			t.Errorf("rewritten repo file = %q, want only the new entry", b)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "test.repo" {
			t.Errorf("repo directory holds %v, want only test.repo", entries)
		}
	})
	t.Run("replaced file keeps mode", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no Unix permission bits")
		}
		path := filepath.Join(t.TempDir(), "test.repo")
		if err := os.WriteFile(path, []byte("[]\n"), 0600); err != nil {
			t.Fatal(err)
		}
		rf := File{Path: path, Entries: newEntries}
		if err := rf.writeOrDelete(); err != nil {
			t.Fatalf("writeOrDelete: %v", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0600 {
			t.Errorf("replaced repo file mode = %v, want 0600", got)
		}
	})
	t.Run("new file gets 0664", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no Unix permission bits")
		}
		path := filepath.Join(t.TempDir(), "test.repo")
		rf := File{Path: path, Entries: newEntries}
		if err := rf.writeOrDelete(); err != nil {
			t.Fatalf("writeOrDelete: %v", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0664 {
			t.Errorf("new repo file mode = %v, want 0664", got)
		}
	})
}

// TestWriteOrDeleteKeepsDirectory checks that emptying a "repo file" that is
// really a directory fails and leaves the directory and its contents alone.
func TestWriteOrDeleteKeepsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dir.repo")
	inside := filepath.Join(path, "keep.txt")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	rf := File{Path: path}
	if err := rf.writeOrDelete(); err == nil {
		t.Error("writeOrDelete on a non-empty directory with no entries = nil, want an error")
	}
	if b, err := os.ReadFile(inside); err != nil || string(b) != "keep" {
		t.Errorf("after writeOrDelete, %q holds %q (error %v), want it intact", inside, b, err)
	}
}

// TestWriteOrDeleteMissing checks that emptying a repo file that doesn't
// exist isn't an error.
func TestWriteOrDeleteMissing(t *testing.T) {
	rf := File{Path: filepath.Join(t.TempDir(), "missing.repo")}
	if err := rf.writeOrDelete(); err != nil {
		t.Errorf("writeOrDelete on a missing file with no entries = %v, want nil", err)
	}
}

func TestAddEntryToFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial []Entry
		add     Entry
		want    []Entry
	}{
		{
			name: "no-existing-file",
			add:  Entry{Name: "foo", URL: "https://gooserver.com/repos/foo"},
			want: []Entry{{Name: "foo", URL: "https://gooserver.com/repos/foo"}},
		},
		{
			name: "non-empty",
			initial: []Entry{
				{Name: "bar", URL: "https://gooserver.com/repos/bar"},
			},
			add: Entry{Name: "foo", URL: "https://gooserver.com/repos/foo"},
			want: []Entry{
				{Name: "bar", URL: "https://gooserver.com/repos/bar"},
				{Name: "foo", URL: "https://gooserver.com/repos/foo"},
			},
		},
		{
			name: "replace-if-already-present",
			initial: []Entry{
				{Name: "foo", URL: "https://gooserver.com/repos/foo", Priority: priority.Default},
				{Name: "bar", URL: "https://gooserver.com/repos/bar"},
			},
			add: Entry{Name: "foo", URL: "https://gooserver.com/repos/foo", Priority: priority.Rollback},
			want: []Entry{
				{Name: "bar", URL: "https://gooserver.com/repos/bar"},
				{Name: "foo", URL: "https://gooserver.com/repos/foo", Priority: priority.Rollback},
			},
		},
		{
			name: "replace-same-name-different-url",
			initial: []Entry{
				{Name: "foo", URL: "https://gooserver.com/repos/foo", Priority: priority.Default},
				{Name: "bar", URL: "https://gooserver.com/repos/bar"},
			},
			add: Entry{Name: "foo", URL: "https://altserver.com/something/else", Priority: priority.Rollback},
			want: []Entry{
				{Name: "bar", URL: "https://gooserver.com/repos/bar"},
				{Name: "foo", URL: "https://altserver.com/something/else", Priority: priority.Rollback},
			},
		},
		{
			name: "replace-different-name-same-url",
			initial: []Entry{
				{Name: "foo", URL: "https://gooserver.com/repos/foo", Priority: priority.Default},
				{Name: "bar", URL: "https://gooserver.com/repos/bar"},
			},
			add: Entry{Name: "newfoo", URL: "https://gooserver.com/repos/foo", Priority: priority.Rollback},
			want: []Entry{
				{Name: "bar", URL: "https://gooserver.com/repos/bar"},
				{Name: "newfoo", URL: "https://gooserver.com/repos/foo", Priority: priority.Rollback},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "test.repo")
			if len(tc.initial) > 0 {
				rf := File{Path: filename, Entries: tc.initial}
				if err := rf.writeOrDelete(); err != nil {
					t.Fatalf("writeOrDelete: %v", err)
				}
			}
			_, err := AddEntryToFile(tc.add, filename)
			if err != nil {
				t.Fatalf("AddEntryToFile(%v, %v): %v", tc.add, filename, err)
			}
			rf, err := unmarshalRepoFile(filename)
			if err != nil {
				t.Fatalf("unmarshalRepoFile(%v): %v", filename, err)
			}
			if diff := cmp.Diff(tc.want, rf.Entries); diff != "" {
				t.Fatalf("AddEntryToFile unexpected diff (-want +got):\n%v", diff)
			}
		})
	}
}

func TestRemoveEntryFromFiles(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initial     []File
		remove      string
		wantChanged []string
		wantFiles   []File
	}{
		{
			name: "remove-from-multiple-files",
			initial: []File{
				{
					Path: "test1.repo",
					Entries: []Entry{
						{Name: "foo", URL: "https://gooserver.com/repos/foo"},
						{Name: "bar", URL: "https://gooserver.com/repos/bar"},
					},
				},
				{
					Path: "test2.repo",
					Entries: []Entry{
						{Name: "baz", URL: "https://gooserver.com/repos/baz"},
						{Name: "foo", URL: "https://gooserver.com/repos/foo"},
						{Name: "hoge", URL: "https://gooserver.com/repos/hoge"},
					},
				},
				{
					Path: "test3.repo",
					Entries: []Entry{
						{Name: "qux", URL: "https://gooserver.com/repos/qux"},
					},
				},
			},
			remove:      "foo",
			wantChanged: []string{"test1.repo", "test2.repo"},
			wantFiles: []File{
				{
					Path: "test1.repo",
					Entries: []Entry{
						{Name: "bar", URL: "https://gooserver.com/repos/bar"},
					},
				},
				{
					Path: "test2.repo",
					Entries: []Entry{
						{Name: "baz", URL: "https://gooserver.com/repos/baz"},
						{Name: "hoge", URL: "https://gooserver.com/repos/hoge"},
					},
				},
				{
					Path: "test3.repo",
					Entries: []Entry{
						{Name: "qux", URL: "https://gooserver.com/repos/qux"},
					},
				},
			},
		},
		{
			name: "remove-file-if-nothing-left",
			initial: []File{
				{Path: "test.repo", Entries: []Entry{{Name: "foo", URL: "https://gooserver.com/repos/foo"}}},
			},
			remove:      "foo",
			wantChanged: []string{"test.repo"},
			wantFiles:   []File{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := t.TempDir()
			for _, rf := range tc.initial {
				rf.Path = filepath.Join(repoDir, rf.Path)
				if err := rf.writeOrDelete(); err != nil {
					t.Fatalf("writeOrDelete: %v", err)
				}
			}
			changed, err := RemoveEntryFromFiles(tc.remove, repoDir)
			if err != nil {
				t.Fatalf("RemoveEntryFromFiles(%v): %v", tc.remove, err)
			}
			// Fix up the paths in tc.wantChanged before comparing.
			var wantChanged []string
			for _, p := range tc.wantChanged {
				wantChanged = append(wantChanged, filepath.Join(repoDir, p))
			}
			if diff := cmp.Diff(wantChanged, changed); diff != "" {
				t.Errorf("changed got unexpected diff (-want +got):\n%v", diff)
			}
			files, err := ConfigFiles(repoDir)
			if err != nil {
				t.Fatalf("ConfigFiles(%v): %v", repoDir, err)
			}
			// Fix up the paths in tc.wantFiles before comparing.
			var wantFiles []File
			for _, rf := range tc.wantFiles {
				rf.Path = filepath.Join(repoDir, rf.Path)
				wantFiles = append(wantFiles, rf)
			}
			if diff := cmp.Diff(wantFiles, files, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ConfigFiles(%v) got unexpected diff (-want +got):\n%v", repoDir, diff)
			}
		})
	}
}

func TestUnmarshalRepoFileMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.repo")
	rf, err := unmarshalRepoFile(p)
	if !os.IsNotExist(err) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unmarshalRepoFile(missing) = %v, want an error matching os.IsNotExist and fs.ErrNotExist", err)
	}
	if rf.Path != p || len(rf.Entries) != 0 {
		t.Errorf("unmarshalRepoFile(missing) = %+v, want File{Path: %q}", rf, p)
	}
}

// TestWriteOrDeleteRemovesWithoutLeftovers checks that emptying a repo file
// removes it without leaving a renamed copy behind, and that the path can be
// written again at once even while a reader holds the old file open.
func TestWriteOrDeleteRemovesWithoutLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.repo")
	if err := os.WriteFile(path, []byte("- name: old\n  url: https://old.example/googet\n"), 0664); err != nil {
		t.Fatal(err)
	}
	r, err := oswrap.OpenShared(path)
	if err != nil {
		t.Fatal(err)
	}
	rf := File{Path: path}
	if err := rf.writeOrDelete(); err != nil {
		r.Close()
		t.Fatalf("writeOrDelete with no entries: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%q still exists after writeOrDelete with no entries (Lstat error: %v)", path, err)
	}
	rf = File{Path: path, Entries: []Entry{{Name: "bar", URL: "https://foo.com/googet/bar"}}}
	if err := rf.writeOrDelete(); err != nil {
		t.Errorf("writing %q again while the old file is open: %v", path, err)
	}
	r.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "test.repo" {
		t.Errorf("repo directory holds %q, want only test.repo", names)
	}
}

func TestReadConfigFile(t *testing.T) {
	errTransient := errors.New("transient")
	errOther := errors.New("other")
	transient := func(err error) bool { return errors.Is(err, errTransient) }
	want := File{Path: "x.repo", Entries: []Entry{{Name: "x", URL: "https://example.com/x"}}}
	for _, tc := range []struct {
		desc      string
		errs      []error // read returns these in turn, then want.
		wantErr   error
		wantCalls int
	}{
		{desc: "success", wantCalls: 1},
		{desc: "transient then success", errs: []error{errTransient}, wantCalls: 2},
		{desc: "transient twice", errs: []error{errTransient, errTransient}, wantErr: errTransient, wantCalls: 2},
		{desc: "other error", errs: []error{errOther}, wantErr: errOther, wantCalls: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			calls := 0
			read := func(f string) (File, error) {
				calls++
				if calls <= len(tc.errs) {
					return File{Path: f}, tc.errs[calls-1]
				}
				return want, nil
			}
			rf, err := readConfigFile("x.repo", read, transient)
			if err != tc.wantErr || calls != tc.wantCalls {
				t.Errorf("readConfigFile() error = %v after %d calls, want %v after %d calls", err, calls, tc.wantErr, tc.wantCalls)
			}
			if err == nil && !cmp.Equal(rf, want) {
				t.Errorf("readConfigFile() = %+v, want %+v", rf, want)
			}
		})
	}
}

func TestSkipLevel(t *testing.T) {
	errTransient := errors.New("transient")
	transient := func(err error) bool { return errors.Is(err, errTransient) }
	always := func(error) bool { return true }
	for _, tc := range []struct {
		desc      string
		err       error
		transient func(error) bool
		want      skipLog
	}{
		{desc: "not exist", err: &fs.PathError{Op: "open", Path: "x.repo", Err: fs.ErrNotExist}, transient: always, want: skipSilently},
		{desc: "transient", err: fmt.Errorf("reading x.repo: %w", errTransient), transient: transient, want: skipInfo},
		{desc: "permission", err: &fs.PathError{Op: "open", Path: "x.repo", Err: fs.ErrPermission}, transient: always, want: skipError},
		{desc: "other", err: errors.New("bad yaml"), transient: transient, want: skipError},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if got := skipLevel(tc.err, tc.transient); got != tc.want {
				t.Errorf("skipLevel(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestConfigFilesReturnsRemainingFiles checks that ConfigFiles still returns
// the other repo files when a listed one can't be found when it is read. It
// doesn't check how the skipped file is logged.
func TestConfigFilesReturnsRemainingFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.repo")
	if err := os.WriteFile(keep, []byte("- name: keep\n  url: https://example.com/keep\n"), 0664); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink is listed by the Glob but fails to read with
	// ENOENT, like a file that a writer removes after the Glob.
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "gone.repo")); err != nil {
		t.Fatal(err)
	}
	got, err := ConfigFiles(dir)
	if err != nil {
		t.Fatalf("ConfigFiles(%q): %v", dir, err)
	}
	want := []File{{Path: keep, Entries: []Entry{{Name: "keep", URL: "https://example.com/keep"}}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ConfigFiles(%q) got unexpected diff (-want +got):\n%v", dir, diff)
	}
}
