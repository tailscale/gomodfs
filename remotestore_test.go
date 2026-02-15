// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/tailscale/gomodfs/store"
	"github.com/tailscale/gomodfs/store/gitstore"
	"github.com/tailscale/gomodfs/store/remotestore"
)

func TestRemoteStore(t *testing.T) {
	// Set up a git-backed server store with testDataTransport for HTTP.
	gitDir := testGitDir(t)
	serverStore := &gitstore.Storage{GitRepo: gitDir}
	addStopGitStoreCleanup(t, serverStore)

	serverFS := &FS{
		Store: serverStore,
		Client: &http.Client{
			Transport: testDataTransport{},
		},
		Logf: t.Logf,
	}

	// Start the HTTP test server with the remotestore handler.
	ts := httptest.NewServer(remotestore.Handler(serverFS))
	t.Cleanup(ts.Close)

	// Create the client store pointing at the test server.
	clientStore := &remotestore.Store{
		BaseURL: ts.URL,
	}
	t.Cleanup(func() { clientStore.Close() })

	ctx := t.Context()

	mv := store.ModuleVersion{
		Module:  "go4.org/mem",
		Version: "v0.0.0-20240501181205-ae6ca9944745",
	}

	// Test GetZipRoot: should trigger on-demand download on the server side.
	mh, err := clientStore.GetZipRoot(ctx, mv)
	if err != nil {
		t.Fatalf("GetZipRoot: %v", err)
	}
	if mh == nil {
		t.Fatal("GetZipRoot returned nil handle")
	}

	// Test CachedModules: should include the module we just fetched.
	cached, err := clientStore.CachedModules(ctx)
	if err != nil {
		t.Fatalf("CachedModules: %v", err)
	}
	if len(cached) != 1 || cached[0] != mv {
		t.Fatalf("CachedModules = %v; want [%v]", cached, mv)
	}

	// Test Stat on a file.
	fi, err := clientStore.Stat(ctx, mh, "mem.go")
	if err != nil {
		t.Fatalf("Stat(mem.go): %v", err)
	}
	if fi.IsDir() {
		t.Fatal("Stat(mem.go): expected regular file, got dir")
	}
	if fi.Size() == 0 {
		t.Fatal("Stat(mem.go): expected non-zero size")
	}

	// Test Stat on a directory.
	fi, err = clientStore.Stat(ctx, mh, "")
	if err != nil {
		t.Fatalf("Stat(root): %v", err)
	}
	if !fi.IsDir() {
		t.Fatal("Stat(root): expected directory")
	}

	// Test Readdir on root.
	ents, err := clientStore.Readdir(ctx, mh, "")
	if err != nil {
		t.Fatalf("Readdir(root): %v", err)
	}
	if len(ents) == 0 {
		t.Fatal("Readdir(root): expected non-empty directory")
	}
	// Verify LICENSE exists in root.
	var foundLicense bool
	for _, ent := range ents {
		if ent.Name == "LICENSE" {
			foundLicense = true
			break
		}
	}
	if !foundLicense {
		t.Fatalf("Readdir(root): expected to find LICENSE, got entries: %v", ents)
	}

	// Test GetFile.
	data, err := clientStore.GetFile(ctx, mh, "LICENSE")
	if err != nil {
		t.Fatalf("GetFile(LICENSE): %v", err)
	}
	if len(data) == 0 {
		t.Fatal("GetFile(LICENSE): expected non-empty data")
	}

	// Test GetFile on non-existent file.
	_, err = clientStore.GetFile(ctx, mh, "nonexistent.go")
	if err == nil {
		t.Fatal("GetFile(nonexistent.go): expected error")
	}

	// Test GetInfoFile (triggers server-side download of .info file).
	infoData, err := clientStore.GetInfoFile(ctx, mv)
	if err != nil {
		t.Fatalf("GetInfoFile: %v", err)
	}
	if len(infoData) == 0 {
		t.Fatal("GetInfoFile: expected non-empty data")
	}

	// Test GetModFile.
	modData, err := clientStore.GetModFile(ctx, mv)
	if err != nil {
		t.Fatalf("GetModFile: %v", err)
	}
	if len(modData) == 0 {
		t.Fatal("GetModFile: expected non-empty data")
	}

	// Test GetZipHash.
	zipHash, err := clientStore.GetZipHash(ctx, mh)
	if err != nil {
		t.Fatalf("GetZipHash: %v", err)
	}
	if len(zipHash) == 0 {
		t.Fatal("GetZipHash: expected non-empty data")
	}

	// Test caching: second GetZipRoot should use cache.
	mh2, err := clientStore.GetZipRoot(ctx, mv)
	if err != nil {
		t.Fatalf("second GetZipRoot: %v", err)
	}
	if mh2 != mh {
		t.Fatal("second GetZipRoot returned different handle; expected cache hit")
	}

	// Test unknown module returns ErrCacheMiss.
	unknownMV := store.ModuleVersion{
		Module:  "example.com/nonexistent",
		Version: "v0.0.1",
	}
	_, err = clientStore.GetZipRoot(ctx, unknownMV)
	if err == nil {
		t.Fatal("GetZipRoot on unknown module: expected error")
	}

	// Test write methods return errReadOnly.
	if err := clientStore.PutModFile(ctx, mv, nil); err == nil {
		t.Fatal("PutModFile: expected error")
	}
	if err := clientStore.PutInfoFile(ctx, mv, nil); err == nil {
		t.Fatal("PutInfoFile: expected error")
	}
	if _, err := clientStore.PutModule(ctx, mv, store.PutModuleData{}); err == nil {
		t.Fatal("PutModule: expected error")
	}
}

func TestRemoteStoreMultipleModules(t *testing.T) {
	gitDir := testGitDir(t)
	serverStore := &gitstore.Storage{GitRepo: gitDir}
	addStopGitStoreCleanup(t, serverStore)

	serverFS := &FS{
		Store: serverStore,
		Client: &http.Client{
			Transport: testDataTransport{},
		},
		Logf: t.Logf,
	}

	ts := httptest.NewServer(remotestore.Handler(serverFS))
	t.Cleanup(ts.Close)

	clientStore := &remotestore.Store{
		BaseURL: ts.URL,
	}
	t.Cleanup(func() { clientStore.Close() })

	ctx := t.Context()

	modules := []store.ModuleVersion{
		{Module: "go4.org/mem", Version: "v0.0.0-20240501181205-ae6ca9944745"},
		{Module: "go4.org", Version: "v0.0.0-20230225012048-214862532bf5"},
		{Module: "github.com/Azure/azure-sdk-for-go/sdk/azcore", Version: "v1.11.0"},
	}

	for _, mv := range modules {
		mh, err := clientStore.GetZipRoot(ctx, mv)
		if err != nil {
			t.Fatalf("GetZipRoot(%v): %v", mv, err)
		}
		if mh == nil {
			t.Fatalf("GetZipRoot(%v): nil handle", mv)
		}
	}

	// Verify CachedModules contains all three.
	cached, err := clientStore.CachedModules(ctx)
	if err != nil {
		t.Fatalf("CachedModules: %v", err)
	}
	gotNames := make([]string, len(cached))
	for i, mv := range cached {
		gotNames[i] = fmt.Sprintf("%s@%s", mv.Module, mv.Version)
	}
	slices.Sort(gotNames)
	wantNames := []string{
		"github.com/Azure/azure-sdk-for-go/sdk/azcore@v1.11.0",
		"go4.org/mem@v0.0.0-20240501181205-ae6ca9944745",
		"go4.org@v0.0.0-20230225012048-214862532bf5",
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("CachedModules = %v; want %v", gotNames, wantNames)
	}
}

func TestRemoteStoreReaddir(t *testing.T) {
	gitDir := testGitDir(t)
	serverStore := &gitstore.Storage{GitRepo: gitDir}
	addStopGitStoreCleanup(t, serverStore)

	serverFS := &FS{
		Store: serverStore,
		Client: &http.Client{
			Transport: testDataTransport{},
		},
		Logf: t.Logf,
	}

	ts := httptest.NewServer(remotestore.Handler(serverFS))
	t.Cleanup(ts.Close)

	clientStore := &remotestore.Store{
		BaseURL: ts.URL,
	}
	t.Cleanup(func() { clientStore.Close() })

	ctx := t.Context()

	// Use go4.org which has subdirectories.
	mv := store.ModuleVersion{
		Module:  "go4.org",
		Version: "v0.0.0-20230225012048-214862532bf5",
	}
	mh, err := clientStore.GetZipRoot(ctx, mv)
	if err != nil {
		t.Fatalf("GetZipRoot: %v", err)
	}

	// Readdir on root should list some dirs.
	rootEnts, err := clientStore.Readdir(ctx, mh, "")
	if err != nil {
		t.Fatalf("Readdir(root): %v", err)
	}

	var gotDirs []string
	for _, ent := range rootEnts {
		if ent.Mode.IsDir() {
			gotDirs = append(gotDirs, ent.Name)
		}
	}
	slices.Sort(gotDirs)

	// go4.org has subdirectories like "media", etc.
	if len(gotDirs) == 0 {
		t.Fatal("Readdir(root): expected directories in go4.org module")
	}

	// Stat a known directory.
	fi, err := clientStore.Stat(ctx, mh, gotDirs[0])
	if err != nil {
		t.Fatalf("Stat(%s): %v", gotDirs[0], err)
	}
	if !fi.IsDir() {
		t.Fatalf("Stat(%s): expected directory", gotDirs[0])
	}

	// GetFile on a directory should return ErrIsDir.
	_, err = clientStore.GetFile(ctx, mh, gotDirs[0])
	if err == nil {
		t.Fatal("GetFile on directory: expected error")
	}
}

func TestRemoteStoreModMapLZ4Required(t *testing.T) {
	gitDir := testGitDir(t)
	serverStore := &gitstore.Storage{GitRepo: gitDir}
	addStopGitStoreCleanup(t, serverStore)

	serverFS := &FS{
		Store: serverStore,
		Client: &http.Client{
			Transport: testDataTransport{},
		},
		Logf: t.Logf,
	}

	ts := httptest.NewServer(remotestore.Handler(serverFS))
	t.Cleanup(ts.Close)

	// Make a direct request without Accept-Encoding: lz4.
	req, err := http.NewRequestWithContext(context.Background(), "GET",
		ts.URL+"/api/v1/modmap/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("expected 406 Not Acceptable, got %s", resp.Status)
	}
}

// Verify the FS type satisfies the FSBackend interface used by the handler.
var _ remotestore.FSBackend = (*FS)(nil)

func TestRemoteStoreFileMode(t *testing.T) {
	gitDir := testGitDir(t)
	serverStore := &gitstore.Storage{GitRepo: gitDir}
	addStopGitStoreCleanup(t, serverStore)

	serverFS := &FS{
		Store: serverStore,
		Client: &http.Client{
			Transport: testDataTransport{},
		},
		Logf: t.Logf,
	}

	ts := httptest.NewServer(remotestore.Handler(serverFS))
	t.Cleanup(ts.Close)

	clientStore := &remotestore.Store{
		BaseURL: ts.URL,
	}
	t.Cleanup(func() { clientStore.Close() })

	ctx := t.Context()

	mv := store.ModuleVersion{
		Module:  "go4.org/mem",
		Version: "v0.0.0-20240501181205-ae6ca9944745",
	}
	mh, err := clientStore.GetZipRoot(ctx, mv)
	if err != nil {
		t.Fatalf("GetZipRoot: %v", err)
	}

	fi, err := clientStore.Stat(ctx, mh, "mem.go")
	if err != nil {
		t.Fatalf("Stat(mem.go): %v", err)
	}
	if fi.Mode()&fs.ModeDir != 0 {
		t.Fatal("Stat(mem.go): file should not be a directory")
	}
	if fi.Mode().Perm() == 0 {
		t.Fatal("Stat(mem.go): file should have non-zero permissions")
	}
}
