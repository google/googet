package repo

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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
