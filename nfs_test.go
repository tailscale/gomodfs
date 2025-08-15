// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/tailscale/gomodfs/store/gitstore"
)

var testDataFiles = map[string]string{
	// Module with a slash in it.
	"https://proxy.golang.org/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.zip":  "go4.org-mem.zip",
	"https://proxy.golang.org/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.info": "go4.org-mem.info",
	"https://proxy.golang.org/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.mod":  "go4.org-mem.mod",

	// Module without a slash in it.
	"https://proxy.golang.org/go4.org/@v/v0.0.0-20230225012048-214862532bf5.zip":  "go4.org.zip",
	"https://proxy.golang.org/go4.org/@v/v0.0.0-20230225012048-214862532bf5.info": "go4.org.info",
	"https://proxy.golang.org/go4.org/@v/v0.0.0-20230225012048-214862532bf5.mod":  "go4.org.mod",

	// Module with an uppercase letter.
	"https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/sdk/azcore/@v/v1.11.0.zip":  "azcore.zip",
	"https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/sdk/azcore/@v/v1.11.0.info": "azcore.info",
	"https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/sdk/azcore/@v/v1.11.0.mod":  "azcore.mod",
}

type testDataTransport struct{}

func (testDataTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != "GET" {
		return nil, fmt.Errorf("unexpected non-GET test request: %s %s", r.Method, r.URL)
	}
	file, ok := testDataFiles[r.URL.String()]
	if !ok {
		return nil, fmt.Errorf("unexpected test request: %s %s", r.Method, r.URL)
	}
	rc, err := os.Open("testdata/" + file)
	if err != nil {
		return nil, err
	}
	fi, err := rc.Stat()
	if err != nil {
		rc.Close()
		return nil, err
	}
	return &http.Response{
		Body:          rc,
		ContentLength: fi.Size(),
		StatusCode:    http.StatusOK,
	}, nil
}

func wantRegSize(t *testing.T, nh *NFSHandler, path string, want int64) {
	t.Helper()
	fi, err := nh.billyFS().Lstat(path)
	if err != nil {
		t.Fatalf("failed to stat file %q: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("expected file mode to be regular file, got %v", fi.Mode())
	}
	if got := fi.Size(); got != want {
		t.Fatalf("file size of %q = %d; want %d", path, got, want)
	}
}

func wantCachedModules(t *testing.T, nh *NFSHandler, want []string) {
	t.Helper()
	mvs, err := nh.fs.Store.CachedModules(context.Background())
	if err != nil {
		t.Fatalf("Failed to get cached modules: %v", err)
	}
	got := make([]string, len(mvs))
	for i, mv := range mvs {
		got[i] = fmt.Sprintf("%s@%s", mv.Module, mv.Version)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("got cached modules %v; want %v", got, want)
	}
}

// pathAndParentPaths returns an iterator yielding first s
// and then all its parents. e.g. "foo/bar/baz", "foo/bar", "foo", "".
func pathAndParentPaths(s string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if !yield(s) {
			return
		}
		for {
			s2 := path.Dir(s)
			if s2 == "." {
				s2 = ""
			}
			if s == s2 || !yield(s2) {
				return
			}
			s = s2
		}
	}
}

func TestNFSHandles(t *testing.T) {
	gitCacheDir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = gitCacheDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	connect := func() *NFSHandler {
		store := &gitstore.Storage{GitRepo: gitCacheDir}
		mfs := &FS{
			Store: store,
			Client: &http.Client{
				Transport: testDataTransport{},
			},
			Logf: t.Logf,
		}
		return mfs.NFSHandler().(*NFSHandler)
	}

	// Two connections. We test that handles from the first are usable with the
	// second one, simulating a reconnect.
	h1 := connect()
	h2 := connect()

	paths := []string{
		"go4.org/mem@v0.0.0-20240501181205-ae6ca9944745/LICENSE",
		"go4.org@v0.0.0-20230225012048-214862532bf5/.travis.yml",
		"go4.org@v0.0.0-20230225012048-214862532bf5/media/heif/heif.go",
		"github.com/!azure/azure-sdk-for-go/sdk/azcore@v1.11.0/arm/runtime/policy_trace_namespace_test.go",

		"cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.info",
		"cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.mod",
		"cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.ziphash",

		"tsgo-linux-amd64/1cd3bf1a6eaf559aa8c00e749289559c884cef09.extracted",
		"tsgo-linux-amd64/1cd3bf1a6eaf559aa8c00e749289559c884cef09/all.bash",
		"tsgo-linux-amd64/1cd3bf1a6eaf559aa8c00e749289559c884cef09/bin/gofmt",
	}
	pathHandle := map[string][]byte{}
	for _, path := range paths {
		for path := range pathAndParentPaths(path) {
			if _, ok := pathHandle[path]; ok {
				continue
			}
			t.Logf("computing handle for %q", path)
			var segs []string
			if path != "" {
				segs = strings.Split(path, "/")
			}
			handle := h1.ToHandle(h1.billyFS(), segs)
			pathHandle[path] = handle
		}
	}

	wantCachedModules(t, h1, []string{})
	// Fault it in.
	wantRegSize(t, h1, "go4.org/mem@v0.0.0-20240501181205-ae6ca9944745/LICENSE", 11358)
	wantCachedModules(t, h1, []string{
		"go4.org/mem@v0.0.0-20240501181205-ae6ca9944745",
	})
	wantRegSize(t, h1, "go4.org@v0.0.0-20230225012048-214862532bf5/media/heif/heif.go", 7434)
	wantCachedModules(t, h1, []string{
		"go4.org@v0.0.0-20230225012048-214862532bf5",
		"go4.org/mem@v0.0.0-20240501181205-ae6ca9944745",
	})
	wantRegSize(t, h1, "github.com/!azure/azure-sdk-for-go/sdk/azcore@v1.11.0/arm/runtime/policy_trace_namespace_test.go", 3215)

	for _, path := range slices.Sorted(maps.Keys(pathHandle)) {
		handle := pathHandle[path]
		t.Logf("checking path %q (which mapped to %02x)", path, handle)
		_, segments, err := h2.FromHandle(handle)
		if err != nil {
			t.Errorf("Failed to get segments from handle of %q: %v", path, err)
			continue
		}
		gotPath := strings.Join(segments, "/")
		if gotPath != path {
			t.Errorf("FromHandle(handle of %q) didn't round trip; went to %q instead", path, gotPath)
		}
	}
}
