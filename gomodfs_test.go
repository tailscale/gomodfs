// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/gomodfs/store"
	"github.com/tailscale/gomodfs/store/gitstore"
)

// test for https://github.com/tailscale/gomodfs/issues/15a
func TestExoticZip(t *testing.T) {
	gitCacheDir := testGitDir(t)
	defer func() {
		if t.Failed() && os.Getenv("CI") != "true" {
			t.Logf("test failed; preserving git cache dir %q and pausing for inspection...", gitCacheDir)
			time.Sleep(5 * time.Minute)
		}
	}()
	st := &gitstore.Storage{GitRepo: gitCacheDir}
	addStopGitStoreCleanup(t, st)
	fs := &FS{
		Store: st,
		Client: &http.Client{
			Transport: testDataTransport{},
		},
		Logf: t.Logf,
	}

	ctx := t.Context()
	mv := store.ModuleVersion{
		Module:  "github.com/bramvdbogaerde/go-scp",
		Version: "v1.4.0",
	}
	mh, err := fs.downloadZip(ctx, mv)
	if err != nil {
		t.Fatalf("downloadZip: %v", err)
	}

	zipHash, err := st.GetZipHash(ctx, mh)
	if err != nil {
		t.Fatalf("GetZipHash: %v", err)
	}
	if g, w := string(zipHash), "h1:jKMwpwCbcX1KyvDbm/PDJuXcMuNVlLGi0Q0reuzjyKY="; g != w {
		t.Fatalf("zip hash = %q; want %q", g, w)
	}

	ents, err := st.Readdir(ctx, mh, "tests/data")
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	var gotBuf strings.Builder
	for i, ent := range ents {
		fmt.Fprintf(&gotBuf, "entry[%d]: %s, %v, size=%v\n", i, ent.Name, ent.Mode, ent.Size)
	}
	got := gotBuf.String()

	want := `entry[0]: Exöt1ç download file.txt.txt, -rw-r--r--, size=23
entry[1]: another_file.txt, -rw-r--r--, size=50
entry[2]: upload_file.txt, -rw-r--r--, size=9
`
	if got != want {
		t.Fatalf("bad directory entries; got:\n%s\nwant:\n%s", got, want)
	}
}

// hostRecordingTransport wraps an http.RoundTripper, recording the
// hosts of attempted requests.
type hostRecordingTransport struct {
	inner http.RoundTripper
	hosts []string
}

func (t *hostRecordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.hosts = append(t.hosts, r.URL.Host)
	return t.inner.RoundTrip(r)
}

func TestModuleProxyFallback(t *testing.T) {
	mv := store.ModuleVersion{
		Module:  "github.com/bramvdbogaerde/go-scp",
		Version: "v1.4.0",
	}
	const wantZipHash = "h1:jKMwpwCbcX1KyvDbm/PDJuXcMuNVlLGi0Q0reuzjyKY="

	newFS := func(t *testing.T, proxies ...string) (*FS, *hostRecordingTransport) {
		st := &gitstore.Storage{GitRepo: testGitDir(t)}
		addStopGitStoreCleanup(t, st)
		tr := &hostRecordingTransport{inner: testDataTransport{}}
		return &FS{
			Store:           st,
			Client:          &http.Client{Transport: tr},
			ModuleProxyURLs: proxies,
			Logf:            t.Logf,
		}, tr
	}

	t.Run("fall-through-to-second", func(t *testing.T) {
		// testDataTransport errors on any URL it doesn't know,
		// simulating an unreachable first proxy.
		fs, tr := newFS(t, "https://bad.proxy.example.com", "https://proxy.golang.org")
		ctx := t.Context()

		mh, err := fs.downloadZip(ctx, mv)
		if err != nil {
			t.Fatalf("downloadZip: %v", err)
		}
		zipHash, err := fs.Store.GetZipHash(ctx, mh)
		if err != nil {
			t.Fatalf("GetZipHash: %v", err)
		}
		if g := string(zipHash); g != wantZipHash {
			t.Fatalf("zip hash = %q; want %q", g, wantZipHash)
		}
		if len(tr.hosts) < 2 || tr.hosts[0] != "bad.proxy.example.com" || tr.hosts[1] != "proxy.golang.org" {
			t.Fatalf("request hosts = %q; want the bad proxy attempted first, then proxy.golang.org", tr.hosts)
		}

		if _, err := fs.downloadModFile(ctx, mv); err != nil {
			t.Fatalf("downloadModFile: %v", err)
		}
		if _, err := fs.downloadInfoFile(ctx, mv); err != nil {
			t.Fatalf("downloadInfoFile: %v", err)
		}
	})

	t.Run("first-success-stops", func(t *testing.T) {
		fs, tr := newFS(t, "https://proxy.golang.org", "https://bad.proxy.example.com")
		if _, err := fs.downloadZip(t.Context(), mv); err != nil {
			t.Fatalf("downloadZip: %v", err)
		}
		for _, h := range tr.hosts {
			if h != "proxy.golang.org" {
				t.Fatalf("request to %q; the second proxy should never be attempted", h)
			}
		}
	})

	t.Run("all-fail", func(t *testing.T) {
		fs, _ := newFS(t, "https://bad1.example.com", "https://bad2.example.com")
		_, err := fs.downloadZip(t.Context(), mv)
		if err == nil {
			t.Fatal("downloadZip succeeded; want error")
		}
		for _, want := range []string{"bad1.example.com", "bad2.example.com"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

func TestModuleProxyURLs(t *testing.T) {
	tests := []struct {
		name string
		fs   *FS
		want []string
	}{
		{"default", &FS{}, []string{"https://proxy.golang.org"}},
		{"single", &FS{ModuleProxyURL: "https://a/"}, []string{"https://a"}},
		{"list", &FS{ModuleProxyURLs: []string{"https://a/", "https://b"}}, []string{"https://a", "https://b"}},
		{"list-wins", &FS{ModuleProxyURL: "https://c", ModuleProxyURLs: []string{"https://a"}}, []string{"https://a"}},
	}
	for _, tt := range tests {
		got := tt.fs.moduleProxyURLs()
		if len(got) != len(tt.want) {
			t.Errorf("%s: got %q; want %q", tt.name, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s: got %q; want %q", tt.name, got, tt.want)
				break
			}
		}
	}
}
