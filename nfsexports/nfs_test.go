// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package nfsexports

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/tailscale/gomodfs"
	"github.com/tailscale/gomodfs/gitrepo"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
	clientnfs "github.com/willscott/go-nfs-client/nfs"
	clientrpc "github.com/willscott/go-nfs-client/nfs/rpc"
)

func exportGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func testRepositoryHandler(t *testing.T) (*gitrepo.NFSHandler, string, string) {
	t.Helper()
	upstream := t.TempDir()
	exportGit(t, upstream, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(upstream, "hello.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	exportGit(t, upstream, "add", "-A")
	exportGit(t, upstream, "commit", "-q", "-m", "initial")
	manager := gitrepo.NewManager(t.TempDir(), map[string]gitrepo.Config{
		"example/repo": {
			RemoteURL: upstream,
		},
	})
	h, err := gitrepo.NewNFSHandler(manager, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return h, upstream, exportGit(t, upstream, "rev-parse", "HEAD")
}

func acquireRepository(t *testing.T, h *gitrepo.NFSHandler, sha string) {
	t.Helper()
	release, err := h.Acquire(t.Context(), "example/repo", sha)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

func TestNFSExportRouting(t *testing.T) {
	repos, _, sha := testRepositoryHandler(t)
	release, err := repos.Acquire(t.Context(), "example/repo", sha)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	mfs := &gomodfs.FS{}
	modules := mfs.NFSHandler()
	h := New(mfs, repos)
	for _, p := range []string{"/", "/gomodcache", "/gomodfs", "/modfs"} {
		status, f, _ := h.Mount(t.Context(), fakeConn{}, nfs.MountRequest{
			Dirpath: []byte(p),
		})
		if status != nfs.MountStatusOk {
			t.Fatalf("Mount(%q): %v", p, status)
		}
		for _, segs := range [][]string{nil, {"cache"}, {".nonexistent"}, {".gomodfs-status"}} {
			got := h.ToHandle(f, segs)
			want := modules.ToHandle(f, segs)
			if !bytes.Equal(got, want) {
				t.Errorf("module handle changed: %x != %x", got, want)
			}
			if _, _, err := h.FromHandle(got); err != nil {
				t.Fatal(err)
			}
			if err := h.InvalidateHandle(f, got); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, p := range []string{"/repos", "/repos/unknown", "/repos/example/repo/" + strings.Repeat("0", 40)} {
		status, _, _ := h.Mount(t.Context(), fakeConn{}, nfs.MountRequest{
			Dirpath: []byte(p),
		})
		if status == nfs.MountStatusOk {
			t.Errorf("invalid repo mount %q fell through to modules", p)
		}
	}
	status, f, _ := h.Mount(t.Context(), fakeConn{}, nfs.MountRequest{
		Dirpath: []byte("/repos/example/repo/" + sha),
	})
	if status != nfs.MountStatusOk {
		t.Fatal(status)
	}
	handle := h.ToHandle(f, []string{"hello.txt"})
	wantHandle := append([]byte("GOMODFS\x01\x01"), repos.ToHandle(f.(repoFS).Filesystem, []string{"hello.txt"})...)
	if !bytes.Equal(handle, wantHandle) {
		t.Fatalf("repository handle = %x; want %x", handle, wantHandle)
	}
	res, err := h.OnNFSRead(t.Context(), handle, 0, 6)
	if err != nil || res == nil || string(res.Data) != "hello\n" {
		t.Fatalf("repository read = %v, %v", res, err)
	}
	decodedFS, decoded, err := h.FromHandle(handle)
	if err != nil || strings.Join(decoded, "/") != "hello.txt" {
		t.Fatalf("FromHandle = %q, %v", decoded, err)
	}
	if got := h.ToHandle(decodedFS, decoded); !bytes.Equal(got, handle) {
		t.Fatalf("round-trip handle = %x; want %x", got, handle)
	}
	if err := h.InvalidateHandle(decodedFS, handle); err != nil {
		t.Fatal(err)
	}
	checkStaleHandle(t, h.InvalidateHandle(nil, handle))
	checkStaleHandle(t, h.InvalidateHandle(decodedFS, modules.ToHandle(nil, nil)))
	if billy.CapabilityCheck(f, billy.WriteCapability) {
		t.Fatal("wrapped repository advertises writes")
	}
	if h.Change(f) != nil {
		t.Fatal("repository supports changes")
	}
	if err := f.Remove("hello.txt"); err == nil {
		t.Fatal("repository supports remove")
	}
	release()
	_, _, err = h.FromHandle(handle)
	checkStaleHandle(t, err)
	_, err = h.OnNFSRead(t.Context(), handle, 0, 6)
	checkStaleHandle(t, err)
}

func checkStaleHandle(t *testing.T, err error) {
	t.Helper()
	var status *nfs.NFSStatusError
	if !errors.As(err, &status) || status.NFSStatus != nfs.NFSStatusStale {
		t.Fatalf("error = %v; want NFS stale handle", err)
	}
}

func TestDecodeHandle(t *testing.T) {
	modules := (&gomodfs.FS{}).NFSHandler()
	magicHash := make([]byte, 64)
	copy(magicHash, "GOMODFS\xff\xff")
	for _, tt := range []struct {
		name    string
		handle  []byte
		kind    handleKind
		payload []byte
	}{
		{"module-root", modules.ToHandle(nil, nil), moduleHandleKind, make([]byte, 64)},
		{"module-magic-hash", magicHash, moduleHandleKind, magicHash},
		{"module-not-exist", modules.ToHandle(nil, []string{".nonexistent"}), moduleHandleKind, []byte("\x00\xFF\x00Nope")},
		{"repo", append([]byte("GOMODFS\x01\x01"), make([]byte, 32)...), repoHandleKind, make([]byte, 32)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kind, payload, err := decodeHandle(tt.handle)
			if err != nil || kind != tt.kind || !bytes.Equal(payload, tt.payload) {
				t.Fatalf("decodeHandle = %d, %x, %v; want %d, %x", kind, payload, err, tt.kind, tt.payload)
			}
		})
	}
}

func TestInvalidExportHandles(t *testing.T) {
	for _, tt := range []struct {
		name   string
		handle []byte
	}{
		{"empty", nil},
		{"bare-hash", make([]byte, 32)},
		{"legacy-uuid", append([]byte("GOMODFS\x01\x01"), make([]byte, 16)...)},
		{"bad-magic", append([]byte("GOMODFX\x01\x01"), make([]byte, 32)...)},
		{"truncated-magic", []byte("GOMODF")},
		{"missing-version", []byte("GOMODFS")},
		{"missing-kind", []byte("GOMODFS\x01")},
		{"unknown-version", append([]byte("GOMODFS\x02\x01"), make([]byte, 32)...)},
		{"unknown-kind", append([]byte("GOMODFS\x01\x02"), make([]byte, 32)...)},
		{"tagged-module", append([]byte("GOMODFS\x01\x00"), make([]byte, 32)...)},
		{"missing-payload", []byte("GOMODFS\x01\x01")},
		{"short-payload", append([]byte("GOMODFS\x01\x01"), make([]byte, 31)...)},
		{"long-payload", append([]byte("GOMODFS\x01\x01"), make([]byte, 33)...)},
		{"max-tagged-length-bad-payload", append([]byte("GOMODFS\x01\x01"), make([]byte, 54)...)},
		{"overlong", append([]byte("GOMODFS\x01\x01"), make([]byte, 56)...)},
		{"bad-module-sentinel", []byte("\x00\xFF\x00Nope!")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := decodeHandle(tt.handle)
			checkStaleHandle(t, err)
		})
	}

	h := &handler{}
	handle := []byte("invalid")
	_, _, err := h.FromHandle(handle)
	checkStaleHandle(t, err)
	_, err = h.OnNFSRead(t.Context(), handle, 0, 6)
	checkStaleHandle(t, err)
	checkStaleHandle(t, h.InvalidateHandle(nil, handle))
}

func TestNFSExportsSharedListener(t *testing.T) {
	repos, upstream, sha := testRepositoryHandler(t)
	acquireRepository(t, repos, sha)
	exportGit(t, upstream, "commit", "--allow-empty", "-qm", "second")
	secondSHA := exportGit(t, upstream, "rev-parse", "HEAD")
	acquireRepository(t, repos, secondSHA)
	modules := &gomodfs.FS{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	server := &nfs.Server{
		Handler: New(modules, repos),
	}
	go server.Serve(ln)
	port := ln.Addr().(*net.TCPAddr).Port
	client, err := clientnfs.DialServiceAtPort("127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	mounter := &clientnfs.Mount{
		Client: client,
	}
	if _, err := mounter.Mount("/repos/unknown", clientrpc.AuthNull); err == nil {
		t.Fatal("unknown checkout mount succeeded")
	}
	var targets []*clientnfs.Target
	for _, p := range []string{"/modfs", "/repos/example/repo/" + sha, "/repos/example/repo/" + secondSHA} {
		target, err := mounter.Mount(p, clientrpc.AuthNull)
		if err != nil {
			t.Fatalf("mount %s: %v", p, err)
		}
		defer target.Close()
		targets = append(targets, target)
	}
	read := func(target *clientnfs.Target, p string) string {
		t.Helper()
		f, err := target.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for i, commit := range []string{sha, secondSHA} {
		target := targets[i+1]
		entries, err := target.ReadDirPlus("")
		if err != nil || len(entries) == 0 {
			t.Fatalf("readdir: %v, %v", entries, err)
		}
		if got := read(target, ".git/HEAD"); got != commit+"\n" {
			t.Errorf("HEAD = %q; want %q", got, commit)
		}
		if got := read(target, "hello.txt"); got != "hello\n" {
			t.Errorf("hello = %q", got)
		}
	}
	if got := read(targets[0], ".gomodfs-status"); !strings.Contains(got, `"filesystem": "gomodfs"`) {
		t.Errorf("module file = %q", got)
	}
}

type fakeConn struct{ net.Conn }

func (fakeConn) RemoteAddr() net.Addr { return &net.TCPAddr{} }
