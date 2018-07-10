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
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestScriptInterpreter(t *testing.T) {
	table := []struct {
		script string
		eitp   string
	}{
		{"/file/path/script.ps1", "powershell"},
		{"/file/path/script.cmd", "cmd"},
		{"/file/path/script.bat", "cmd"},
	}
	for _, tt := range table {
		itp, err := scriptInterpreter(tt.script)
		if err != nil {
			t.Errorf("error parsing interpreter: %v", err)
		}
		if itp != tt.eitp {
			t.Errorf("did not get expected interpreter: got %v, want %v", itp, tt.eitp)
		}
	}
}

func TestBadScriptInterpreter(t *testing.T) {
	if _, err := scriptInterpreter("/file/path/script.ext"); err == nil {
		t.Errorf("got no error from scriptInterpreter when processing bad extension, want error")
	}
	if _, err := scriptInterpreter("/file/path/script"); err == nil {
		t.Errorf("got no error from scriptInterpreter when processing no extension, want error")
	}
}

func TestContainsInt(t *testing.T) {
	table := []struct {
		a     int
		slice []int
		want  bool
	}{
		{1, []int{1, 2}, true},
		{3, []int{1, 2}, false},
	}
	for _, tt := range table {
		if got, want := ContainsInt(tt.a, tt.slice), tt.want; got != want {
			t.Errorf("Contains(%d, %v) incorrect return: got %v, want %t", tt.a, tt.slice, got, want)
		}
	}
}

func TestContainsString(t *testing.T) {
	table := []struct {
		a     string
		slice []string
		want  bool
	}{
		{"a", []string{"a", "b"}, true},
		{"c", []string{"a", "b"}, false},
	}
	for _, tt := range table {
		if got, want := ContainsString(tt.a, tt.slice), tt.want; got != want {
			t.Errorf("Contains(%s, %v) incorrect return: got %v, want %t", tt.a, tt.slice, got, want)
		}
	}
}

func randString(runes []rune, min, max int) string {
	s := make([]rune, rand.Intn(1+max-min)+min)
	for i := range s {
		s[i] = runes[rand.Intn(len(runes))]
	}
	return string(s)
}

func TestSplitGCSUrl(t *testing.T) {
	rand.Seed(time.Now().UnixNano())
	const alphanum = "abcdefghijklmnopqrstuvwxyz0123456789"
	objChars := alphanum + "ABCDEFGHIJKLMNOPQRSTUVWXYZ-_.~@%^=+"
	bucket := randString([]rune(alphanum), 1, 1) + randString([]rune(alphanum+"-_."), 0, 61) + randString([]rune(alphanum), 1, 1)
	object := randString([]rune(objChars+"/"), 0, 49) + randString([]rune(objChars), 1, 1)

	var domains = []string{
		`storage.cloud.google.com`,
		`storage.googleapis.com`,
		`commondatastorage.googleapis.com`,
	}
	var urls, urlsNoObjs []string
	for i := range domains {
		for _, s := range []string{"", "s"} {
			// Without Objects
			urlsNoObjs = append(urlsNoObjs, fmt.Sprintf("http%s://%s/%s", s, domains[i], bucket))
			urlsNoObjs = append(urlsNoObjs, fmt.Sprintf("http%s://%s/%s", s, strings.ToUpper(domains[i]), bucket))
			// With objects
			urls = append(urls, fmt.Sprintf("http%s://%s/%s/%s", s, domains[i], bucket, object))
			urls = append(urls, fmt.Sprintf("http%s://%s/%s/%s", s, strings.ToUpper(domains[i]), bucket, object))
		}
	}
	urls = append(urls, fmt.Sprintf(`http://%s.storage.googleapis.com/%s`, bucket, object))
	urls = append(urls, fmt.Sprintf(`http://%s.Storage.googleapis.COM/%s`, bucket, object))
	urls = append(urls, fmt.Sprintf(`https://%s.storage.googleapis.com/%s`, bucket, object))
	urls = append(urls, fmt.Sprintf(`https://%s.Storage.googleapis.COM/%s`, bucket, object))
	urlsNoObjs = append(urlsNoObjs, fmt.Sprintf(`gs://%s`, bucket))
	urls = append(urls, fmt.Sprintf(`gs://%s/%s`, bucket, object))
	for i := range urls {
		urls = append(urls, urls[i]+"/")
	}
	for _, url := range urls {
		ok := true
		isGCSUrl, bkt, obj := SplitGCSUrl(url)
		if !isGCSUrl {
			t.Errorf("Failed to parse '%s', expecting bucket='%s', object='%s'", url, bucket, object)
			ok = false
		} else {
			if bkt != bucket {
				t.Errorf("Parsed bucket '%s' from '%s', expecting '%s'", bkt, url, bucket)
				ok = false
			}
			if obj != object {
				t.Errorf("Parsed object '%s' from '%s', expecting '%s'", obj, url, object)
				ok = false
			}
		}
		if ok {
			t.Logf("Successfully parsed object='%s', bucket='%s' from '%s'", obj, bkt, url)
		}
	}
	for _, url := range urlsNoObjs {
		ok := true
		isGCSUrl, bkt, obj := SplitGCSUrl(url)
		if !isGCSUrl {
			t.Errorf("Failed to parse '%s', expecting bucket='%s', no object", url, bucket)
			ok = false
		} else {
			if bkt != bucket {
				t.Errorf("Parsed bucket '%s' from '%s', expecting '%s'", bkt, url, bucket)
				ok = false
			}
			if obj != "" {
				t.Errorf("Parsed object '%s' from '%s', expecting an empty string", obj, url)
				ok = false
			}
		}
		if ok {
			t.Logf("Successfully parsed object='%s', bucket='%s' from '%s'", obj, bkt, url)
		}
	}
}
