// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
)

func testRepositoryHandler(t *testing.T) (*NFSHandler, string, string) {
	t.Helper()
	upstream := t.TempDir()
	git(t, upstream, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(upstream, "hello.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-q", "-m", "initial")
	sha := git(t, upstream, "rev-parse", "HEAD")
	manager := NewManager(t.TempDir(), map[string]Config{
		"example/repo": {
			RemoteURL: upstream,
		},
	})
	h, err := NewNFSHandler(manager, 1234, 5678)
	if err != nil {
		t.Fatal(err)
	}
	return h, upstream, sha
}

func acquireRepository(t *testing.T, h *NFSHandler, sha string) func() {
	t.Helper()
	release, err := h.Acquire(t.Context(), "example/repo", sha)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

func TestNFSRepositoryLifetime(t *testing.T) {
	h, _, sha := testRepositoryHandler(t)
	req := nfs.MountRequest{
		Dirpath: []byte("/repos/example/repo/" + sha),
	}
	if status, _, _ := h.Mount(t.Context(), nil, req); status == nfs.MountStatusOk {
		t.Fatal("unacquired checkout is mountable")
	}
	first := acquireRepository(t, h, sha)
	second := acquireRepository(t, h, sha)
	_, f, _ := h.Mount(t.Context(), nil, req)
	file := h.ToHandle(f, []string{"hello.txt"})
	h.ToHandle(f, []string{"another-path"})
	first()
	first()
	_, f2, _ := h.Mount(t.Context(), nil, req)
	if got := h.ToHandle(f2, []string{"hello.txt"}); !bytes.Equal(got, file) {
		t.Fatal("same checkout/path has different handles")
	}
	if _, segs, err := h.FromHandle(file); err != nil || strings.Join(segs, "/") != "hello.txt" {
		t.Fatalf("live handle = %q, %v", segs, err)
	}
	second()
	for _, handle := range [][]byte{file, h.ToHandle(f, nil), h.ToHandle(nil, nil)} {
		_, _, err := h.FromHandle(handle)
		var status *nfs.NFSStatusError
		if !errors.As(err, &status) || status.NFSStatus != nfs.NFSStatusStale {
			t.Fatalf("released handle error = %v; want NFS stale handle", err)
		}
	}
	if len(h.handles) != 0 || len(h.exports) != 0 {
		t.Fatal("last release retained checkout state")
	}
	if status, _, _ := h.Mount(t.Context(), nil, req); status == nfs.MountStatusOk {
		t.Fatal("released checkout is still mountable")
	}
	acquireRepository(t, h, sha)
	_, f3, _ := h.Mount(t.Context(), nil, req)
	if !bytes.Equal(file, h.ToHandle(f3, []string{"hello.txt"})) {
		t.Fatal("reacquire changed the handle")
	}
	if _, segs, err := h.FromHandle(file); err != nil || strings.Join(segs, "/") != "hello.txt" {
		t.Fatalf("reacquired handle = %q, %v", segs, err)
	}

	// A fresh handler produces the same handles, but cannot resolve them
	// until ToHandle has registered the path in its own reverse index.
	fresh, err := NewNFSHandler(h.repos, 999, 999)
	if err != nil {
		t.Fatal(err)
	}
	acquireRepository(t, fresh, sha)
	_, f4, _ := fresh.Mount(t.Context(), nil, req)
	if _, _, err := fresh.FromHandle(file); err == nil {
		t.Fatal("fresh handler resolved an unregistered handle")
	}
	if !bytes.Equal(file, fresh.ToHandle(f4, []string{"hello.txt"})) {
		t.Fatal("fresh handler changed the handle")
	}
	if _, segs, err := fresh.FromHandle(file); err != nil || strings.Join(segs, "/") != "hello.txt" {
		t.Fatalf("fresh handler registered handle = %q, %v", segs, err)
	}
}

func TestNFSRepositoryHandleIdentity(t *testing.T) {
	h := &NFSHandler{
		handles: map[[sha256.Size]byte]repoPathHandle{},
	}
	seen := map[string]bool{}
	for _, tt := range []struct {
		repo, commit, path string
	}{
		{"example/repo", strings.Repeat("a", 40), "hello.txt"},
		{"example/other", strings.Repeat("a", 40), "hello.txt"},
		{"example/repo", strings.Repeat("b", 40), "hello.txt"},
		{"example/repo", strings.Repeat("a", 40), "dir/hello.txt"},
		{"example/repo", strings.Repeat("a", 40), ""},
		{"example/repo", strings.Repeat("a", 40), ".git/HEAD"},
	} {
		e := &repoExport{
			co: &checkout{
				repo: &repository{
					name: tt.repo,
				},
				commit: tt.commit,
			},
			refs:  1,
			paths: map[string][sha256.Size]byte{},
		}
		var segs []string
		if tt.path != "" {
			segs = strings.Split(tt.path, "/")
		}
		got := h.ToHandle(h.filesystem(e), segs)
		want := sha256.Sum256([]byte(tt.repo + "\x00" + tt.commit + "\x00" + tt.path))
		if !bytes.Equal(got, want[:]) {
			t.Fatalf("handle(%q, %q, %q) = %x; want %x", tt.repo, tt.commit, tt.path, got, want)
		}
		if seen[string(got)] {
			t.Fatal("different identities produced the same handle")
		}
		seen[string(got)] = true
		if _, decoded, err := h.FromHandle(got); err != nil || strings.Join(decoded, "/") != tt.path {
			t.Fatalf("FromHandle = %q, %v; want %q", decoded, err, tt.path)
		}
	}
	for _, malformed := range [][]byte{nil, make([]byte, 16), make([]byte, 31), make([]byte, 33), make([]byte, 32)} {
		_, _, err := h.FromHandle(malformed)
		var status *nfs.NFSStatusError
		if !errors.As(err, &status) || status.NFSStatus != nfs.NFSStatusStale {
			t.Fatalf("FromHandle(%x) error = %v; want NFS stale handle", malformed, err)
		}
	}
}

func TestNFSRepositoryConcurrentGuests(t *testing.T) {
	h, _, sha := testRepositoryHandler(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 4 {
				release, err := h.Acquire(t.Context(), "example/repo", sha)
				if err != nil {
					t.Error(err)
					return
				}
				_, f, _ := h.Mount(t.Context(), nil, nfs.MountRequest{
					Dirpath: []byte("/repos/example/repo/" + sha),
				})
				handle := h.ToHandle(f, []string{"hello.txt"})
				if _, _, err := h.FromHandle(handle); err != nil {
					t.Error(err)
				}
				release()
			}
		})
	}
	wg.Wait()
	if len(h.handles) != 0 || len(h.exports) != 0 {
		t.Fatal("guest teardown leaked state")
	}
}

func TestNFSRepositoryAcquireValidation(t *testing.T) {
	h, _, sha := testRepositoryHandler(t)
	for _, tt := range []struct {
		repo   string
		commit string
	}{
		{"example/repo", "not-a-sha"},
		{"unknown/repo", sha},
		{"../repo", sha},
	} {
		if release, err := h.Acquire(t.Context(), tt.repo, tt.commit); err == nil {
			release()
			t.Errorf("Acquire(%q, %q) succeeded", tt.repo, tt.commit)
		}
	}
	if len(h.exports) != 0 || len(h.handles) != 0 {
		t.Fatal("failed acquire left state")
	}
}

func TestNFSRepositoryRead(t *testing.T) {
	h, _, sha := testRepositoryHandler(t)
	acquireRepository(t, h, sha)
	_, f, _ := h.Mount(t.Context(), nil, nfs.MountRequest{
		Dirpath: []byte("/repos/example/repo/" + sha),
	})
	fi, err := f.Lstat("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	wantAttr := nfs.ToFileAttribute(fi, "hello.txt")
	handle := h.ToHandle(f, []string{"hello.txt"})
	for _, tt := range []struct {
		offset uint64
		count  uint32
		want   string
		eof    bool
	}{
		{0, 0, "", false},
		{0, 6, "hello\n", true},
		{1, 3, "ell", false},
		{4, 100, "o\n", true},
		{6, 100, "", true},
		{^uint64(0), 100, "", true},
	} {
		res, err := h.OnNFSRead(t.Context(), handle, tt.offset, tt.count)
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Data) != tt.want || res.EOF != tt.eof {
			t.Errorf("read(%d, %d) = %q, EOF %v; want %q, %v", tt.offset, tt.count, res.Data, res.EOF, tt.want, tt.eof)
		}
		if res.Attr.Fileid != wantAttr.Fileid || res.Attr.UID != 1234 || res.Attr.GID != 5678 {
			t.Errorf("read attributes disagree with lookup/owner: %+v", res.Attr)
		}
	}
}
