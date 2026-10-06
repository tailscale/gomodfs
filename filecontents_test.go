// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"bytes"
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/tailscale/gomodfs/store"
	"github.com/tailscale/gomodfs/store/gitstore"
)

// getFileCountingStore is a store.Store that counts GetFile calls.
type getFileCountingStore struct {
	store.Store
	getFiles atomic.Int64
}

func (s *getFileCountingStore) GetFile(ctx context.Context, h store.ModHandle, path string) ([]byte, error) {
	s.getFiles.Add(1)
	return s.Store.GetFile(ctx, h, path)
}

func TestGetFileContentsCache(t *testing.T) {
	gs := &gitstore.Storage{GitRepo: testGitDir(t)}
	addStopGitStoreCleanup(t, gs)
	cs := &getFileCountingStore{Store: gs}
	fs := &FS{
		Store:  cs,
		Client: &http.Client{Transport: testDataTransport{}},
		Logf:   t.Logf,
	}
	ctx := t.Context()
	mv := store.ModuleVersion{
		Module:  "go4.org/mem",
		Version: "v0.0.0-20240501181205-ae6ca9944745",
	}
	mh, err := fs.getZipRoot(ctx, mv)
	if err != nil {
		t.Fatal(err)
	}

	read := func(path string) []byte {
		t.Helper()
		got, err := fs.getFileContents(ctx, mv, mh, path)
		if err != nil {
			t.Fatalf("getFileContents(%q): %v", path, err)
		}
		want, err := gs.GetFile(ctx, mh, path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("getFileContents(%q) = %q; want %q", path, got, want)
		}
		return got
	}
	wantGetFiles := func(want int64) {
		t.Helper()
		if got := cs.getFiles.Load(); got != want {
			t.Errorf("store GetFile calls = %d; want %d", got, want)
		}
	}

	for range 3 {
		read("fold.go")
	}
	wantGetFiles(1)

	// With room for only one of the two files, reading the other evicts it.
	fs2 := &FS{Store: cs, FileCacheSize: int64(len(read("fold.go")))}
	cs.getFiles.Store(0)
	for _, path := range []string{"fold.go", "fold.go", "fields.go", "fold.go"} {
		if _, err := fs2.getFileContents(ctx, mv, mh, path); err != nil {
			t.Fatal(err)
		}
	}
	wantGetFiles(3)
}
