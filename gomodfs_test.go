// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/gomodfs/store"
	"github.com/tailscale/gomodfs/store/gitstore"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
	"github.com/tailscale/gomodfs/testing/nfsmount/mount"
)

// test for https://github.com/tailscale/gomodfs/issues/15
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

func TestGoModDownload(t *testing.T) {
	gitCacheDir := testGitDir(t)
	st := &gitstore.Storage{GitRepo: gitCacheDir}
	addStopGitStoreCleanup(t, st)
	fs := &FS{
		Store: st,
		Client: &http.Client{
			Transport: testDataTransport{},
		},
		Logf: t.Logf,
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Failed to listen on NFS port %s: %v", ":0", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() {
		ln.Close()
	})
	nfsListenAddr := ln.Addr()
	t.Logf("NFS server listening at %s", nfsListenAddr)
	nfsSrv := &nfs.Server{
		Context: t.Context(),
		Handler: fs.NFSHandler(),
	}
	go nfsSrv.Serve(ln)

	dir := mount.Mount(port)
	t.Cleanup(func() {
		cmd := exec.Command("sudo", "umount", "-l", dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("umount %s failed: %v\nOutput:\n%s", dir, err, out)
		}
	})
	cmd := exec.CommandContext(t.Context(), "go", "mod", "verify")
	cmd.Env = append(os.Environ(), "GOMODCACHE="+dir)
	wd, _ := os.Getwd()
	cmd.Dir = filepath.Join(wd, "testdata", "exoticmod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod download failed: %v\nOutput:\n%s", err, out)
	}
	t.Logf("go mod download output:\n%s", out)
}
