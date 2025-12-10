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
