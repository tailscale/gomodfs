// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package remotestore

import (
	"io/fs"
	"net/http"
	"slices"
	"testing"

	"github.com/tailscale/gomodfs/store"
)

func TestBuildDirMap(t *testing.T) {
	tests := []struct {
		name  string
		files ModVersionMap
		// wantDirs maps dir path to expected entries (sorted by name).
		wantDirs map[string][]store.Dirent
	}{
		{
			name:     "empty",
			files:    ModVersionMap{},
			wantDirs: map[string][]store.Dirent{},
		},
		{
			name: "root-file", // single file, no subdirectories
			files: ModVersionMap{
				"README.md": {Size: 100, Mode: 0644},
			},
			wantDirs: map[string][]store.Dirent{
				"": {{Name: "README.md", Mode: 0644, Size: 100}},
			},
		},
		{
			name: "deep-single", // a/b/c/d.txt; intermediates must all appear
			files: ModVersionMap{
				"a/b/c/d.txt": {Size: 42, Mode: 0644},
			},
			wantDirs: map[string][]store.Dirent{
				"":      {{Name: "a", Mode: fs.ModeDir}},
				"a":     {{Name: "b", Mode: fs.ModeDir}},
				"a/b":   {{Name: "c", Mode: fs.ModeDir}},
				"a/b/c": {{Name: "d.txt", Mode: 0644, Size: 42}},
			},
		},
		{
			name: "shared-prefix", // a/b/c/d.txt + a/b/file.txt share a/b
			files: ModVersionMap{
				"a/b/c/d.txt":  {Size: 42, Mode: 0644},
				"a/b/file.txt": {Size: 99, Mode: 0755},
			},
			wantDirs: map[string][]store.Dirent{
				"": {{Name: "a", Mode: fs.ModeDir}},
				"a": {
					{Name: "b", Mode: fs.ModeDir},
				},
				"a/b": {
					{Name: "c", Mode: fs.ModeDir},
					{Name: "file.txt", Mode: 0755, Size: 99},
				},
				"a/b/c": {{Name: "d.txt", Mode: 0644, Size: 42}},
			},
		},
		{
			name: "multi-root", // files and dirs at root level
			files: ModVersionMap{
				"cmd/main.go": {Size: 100, Mode: 0644},
				"lib/util.go": {Size: 200, Mode: 0644},
				"README.md":   {Size: 50, Mode: 0644},
			},
			wantDirs: map[string][]store.Dirent{
				"": {
					{Name: "README.md", Mode: 0644, Size: 50},
					{Name: "cmd", Mode: fs.ModeDir},
					{Name: "lib", Mode: fs.ModeDir},
				},
				"cmd": {{Name: "main.go", Mode: 0644, Size: 100}},
				"lib": {{Name: "util.go", Mode: 0644, Size: 200}},
			},
		},
		{
			name: "mixed-depths", // files at every level of a/b/c
			files: ModVersionMap{
				"a/top.go":        {Size: 10, Mode: 0644},
				"a/b/mid.go":      {Size: 20, Mode: 0644},
				"a/b/c/bottom.go": {Size: 30, Mode: 0755},
			},
			wantDirs: map[string][]store.Dirent{
				"": {{Name: "a", Mode: fs.ModeDir}},
				"a": {
					{Name: "b", Mode: fs.ModeDir},
					{Name: "top.go", Mode: 0644, Size: 10},
				},
				"a/b": {
					{Name: "c", Mode: fs.ModeDir},
					{Name: "mid.go", Mode: 0644, Size: 20},
				},
				"a/b/c": {{Name: "bottom.go", Mode: 0755, Size: 30}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDirMap(tt.files)

			// Check we have exactly the expected directory keys.
			if len(got) != len(tt.wantDirs) {
				t.Errorf("got %d dirs, want %d", len(got), len(tt.wantDirs))
				t.Logf("got dirs: %v", dirKeys(got))
				t.Logf("want dirs: %v", dirKeys(tt.wantDirs))
			}

			for dir, wantEnts := range tt.wantDirs {
				gotEnts, ok := got[dir]
				if !ok {
					t.Errorf("missing directory %q", dir)
					continue
				}
				sortDirents(gotEnts)
				sortDirents(wantEnts)
				if !slices.Equal(gotEnts, wantEnts) {
					t.Errorf("dir %q:\n  got:  %v\n  want: %v", dir, gotEnts, wantEnts)
				}
			}

			for dir := range got {
				if _, ok := tt.wantDirs[dir]; !ok {
					t.Errorf("unexpected directory %q with entries %v", dir, got[dir])
				}
			}
		})
	}
}

func TestAcceptsLZ4(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{"exact", "lz4", true},
		{"with-gzip", "gzip, lz4", true},
		{"weighted", "gzip;q=0.5, lz4;q=1.0", true},
		{"weighted-space", "lz4 ; q=1.0", true},
		{"missing", "gzip, br", false},
		{"empty", "", false},
		{"upper", "LZ4", true},
		{"mixed-case", "Lz4;q=1", true},
		{"prefix-mismatch", "lz4frame", false}, // not a match
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{"Accept-Encoding": {tt.header}}}
			if got := acceptsLZ4(r); got != tt.want {
				t.Errorf("acceptsLZ4(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func sortDirents(d []store.Dirent) {
	slices.SortFunc(d, func(a, b store.Dirent) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
}

func dirKeys(m map[string][]store.Dirent) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
