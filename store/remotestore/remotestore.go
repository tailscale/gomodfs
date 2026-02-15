// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package remotestore implements a store.Store backed by an HTTP remote server,
// and an HTTP handler that serves the store API from a gomodfs.FS backend.
package remotestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	"github.com/pierrec/lz4/v4"
	"github.com/tailscale/gomodfs/store"
	"golang.org/x/sync/singleflight"
)

var errReadOnly = errors.New("remotestore: read-only store")

// Store is a store.Store implementation backed by an HTTP remote server.
type Store struct {
	BaseURL string       // e.g. "http://localhost:8090"
	Client  *http.Client // or nil for http.DefaultClient

	sf singleflight.Group

	mu      sync.Mutex
	modmaps map[store.ModuleVersion]*remoteModHandle // cached modmap per MV
	tmpDir  string                                   // lazy-created temp dir for large files
}

// Close cleans up temporary files. It should be called when the store is no longer needed.
func (s *Store) Close() error {
	s.mu.Lock()
	dir := s.tmpDir
	s.mu.Unlock()
	if dir != "" {
		return os.RemoveAll(dir)
	}
	return nil
}

func (s *Store) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// remoteModHandle is the ModHandle returned by GetZipRoot. It holds
// the parsed modmap (file metadata) and derived directory entries.
type remoteModHandle struct {
	mv    store.ModuleVersion
	files ModVersionMap             // path -> FileMeta
	dirs  map[string][]store.Dirent // dir path -> entries
}

// CachedModules returns module versions that have been fetched via GetZipRoot.
func (s *Store) CachedModules(_ context.Context) ([]store.ModuleVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mvs := make([]store.ModuleVersion, 0, len(s.modmaps))
	for mv := range s.modmaps {
		mvs = append(mvs, mv)
	}
	return mvs, nil
}

// GetZipRoot fetches the modmap from the server (or returns a cached one),
// and returns a handle that can be used with Stat, Readdir, and GetFile.
func (s *Store) GetZipRoot(ctx context.Context, mv store.ModuleVersion) (store.ModHandle, error) {
	s.mu.Lock()
	if h, ok := s.modmaps[mv]; ok {
		s.mu.Unlock()
		return h, nil
	}
	s.mu.Unlock()

	vi, err, _ := s.sf.Do("modmap:"+mv.Module+"@"+mv.Version, func() (any, error) {
		return s.fetchModMap(ctx, mv)
	})
	if err != nil {
		return nil, err
	}
	return vi.(*remoteModHandle), nil
}

// fetchModMap fetches a module's file metadata map from the remote server,
// decompresses and decodes it, caches the result, and returns it.
func (s *Store) fetchModMap(ctx context.Context, mv store.ModuleVersion) (*remoteModHandle, error) {
	// Double-check cache inside singleflight.
	s.mu.Lock()
	if h, ok := s.modmaps[mv]; ok {
		s.mu.Unlock()
		return h, nil
	}
	s.mu.Unlock()

	escaped, err := mvToEscapedPath(mv)
	if err != nil {
		return nil, err
	}

	reqURL := s.BaseURL + modmapPrefix + escaped
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "lz4")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, store.ErrCacheMiss
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remotestore: modmap %v: %s", mv, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Decompress LZ4 if server sent it compressed.
	if resp.Header.Get("Content-Encoding") == "lz4" {
		r := lz4.NewReader(bytes.NewReader(body))
		body, err = io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("remotestore: lz4 decompress: %w", err)
		}
	}

	var fmap ModVersionMap
	if err := json.Unmarshal(body, &fmap); err != nil {
		return nil, fmt.Errorf("remotestore: json decode modmap: %w", err)
	}

	h := &remoteModHandle{
		mv:    mv,
		files: fmap,
		dirs:  buildDirMap(fmap),
	}

	s.mu.Lock()
	if s.modmaps == nil {
		s.modmaps = make(map[store.ModuleVersion]*remoteModHandle)
	}
	s.modmaps[mv] = h
	s.mu.Unlock()

	return h, nil
}

// Stat returns file info for a path within a module version.
func (s *Store) Stat(_ context.Context, h store.ModHandle, filePath string) (fs.FileInfo, error) {
	rmh := h.(*remoteModHandle)

	if filePath == "" {
		// Root directory.
		return &remoteFileInfo{name: ".", mode: fs.ModeDir, isDir: true}, nil
	}

	// Check if it's a regular file.
	if fm, ok := rmh.files[filePath]; ok {
		name := path.Base(filePath)
		return &remoteFileInfo{name: name, size: fm.Size, mode: fm.Mode}, nil
	}

	// Check if it's a directory.
	if _, ok := rmh.dirs[filePath]; ok {
		name := path.Base(filePath)
		return &remoteFileInfo{name: name, mode: fs.ModeDir, isDir: true}, nil
	}

	return nil, os.ErrNotExist
}

// Readdir returns directory entries for a path within a module version.
func (s *Store) Readdir(_ context.Context, h store.ModHandle, dirPath string) ([]store.Dirent, error) {
	rmh := h.(*remoteModHandle)
	ents, ok := rmh.dirs[dirPath]
	if !ok {
		if dirPath == "" {
			return nil, nil // empty root
		}
		return nil, os.ErrNotExist
	}
	return ents, nil
}

// GetFile fetches a file's contents from the server.
func (s *Store) GetFile(ctx context.Context, h store.ModHandle, filePath string) ([]byte, error) {
	rmh := h.(*remoteModHandle)

	// Verify the file exists in the modmap.
	if _, ok := rmh.files[filePath]; !ok {
		if _, ok := rmh.dirs[filePath]; ok {
			return nil, store.ErrIsDir
		}
		return nil, os.ErrNotExist
	}

	escaped, err := mvToEscapedPath(rmh.mv)
	if err != nil {
		return nil, err
	}

	reqURL := s.BaseURL + filePrefix + escaped + "?path=" + url.QueryEscape(filePath)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remotestore: get file %s in %v: %s", filePath, rmh.mv, resp.Status)
	}

	return io.ReadAll(resp.Body)
}

// GetInfoFile returns the .info metadata file for a module version.
func (s *Store) GetInfoFile(ctx context.Context, mv store.ModuleVersion) ([]byte, error) {
	return s.getMetaFile(ctx, mv, "info")
}

// GetModFile returns the .mod metadata file for a module version.
func (s *Store) GetModFile(ctx context.Context, mv store.ModuleVersion) ([]byte, error) {
	return s.getMetaFile(ctx, mv, "mod")
}

// GetZipHash returns the .ziphash metadata file for a module version.
func (s *Store) GetZipHash(ctx context.Context, h store.ModHandle) ([]byte, error) {
	rmh := h.(*remoteModHandle)
	return s.getMetaFile(ctx, rmh.mv, "ziphash")
}

func (s *Store) getMetaFile(ctx context.Context, mv store.ModuleVersion, ext string) ([]byte, error) {
	escaped, err := mvToEscapedPath(mv)
	if err != nil {
		return nil, err
	}

	reqURL := s.BaseURL + metaPrefix + escaped + "." + ext
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, store.ErrCacheMiss
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remotestore: get %s for %v: %s", ext, mv, resp.Status)
	}

	return io.ReadAll(resp.Body)
}

// PutModule is not supported on remotestore.
func (s *Store) PutModule(context.Context, store.ModuleVersion, store.PutModuleData) (store.ModHandle, error) {
	return nil, errReadOnly
}

// PutModFile is not supported on remotestore.
func (s *Store) PutModFile(context.Context, store.ModuleVersion, []byte) error {
	return errReadOnly
}

// PutInfoFile is not supported on remotestore.
func (s *Store) PutInfoFile(context.Context, store.ModuleVersion, []byte) error {
	return errReadOnly
}

// buildDirMap derives directory entries from a flat file map.
// It walks all file paths and registers each parent directory.
func buildDirMap(files ModVersionMap) map[string][]store.Dirent {
	dirs := make(map[string][]store.Dirent)
	dirSet := make(map[string]map[string]bool) // dir -> set of child names already added

	for filePath, fm := range files {
		// Add the file itself to its parent directory.
		dir := path.Dir(filePath)
		if dir == "." {
			dir = ""
		}
		name := path.Base(filePath)

		if dirSet[dir] == nil {
			dirSet[dir] = make(map[string]bool)
		}
		if !dirSet[dir][name] {
			dirSet[dir][name] = true
			dirs[dir] = append(dirs[dir], store.Dirent{
				Name: name,
				Mode: fm.Mode,
				Size: fm.Size,
			})
		}

		// Walk up the directory tree to ensure parent directories exist.
		child := dir
		for child != "" {
			parent := path.Dir(child)
			if parent == "." {
				parent = ""
			}
			childName := path.Base(child)

			if dirSet[parent] == nil {
				dirSet[parent] = make(map[string]bool)
			}
			if !dirSet[parent][childName] {
				dirSet[parent][childName] = true
				dirs[parent] = append(dirs[parent], store.Dirent{
					Name: childName,
					Mode: fs.ModeDir,
				})
			}
			child = parent
		}
	}

	return dirs
}

// remoteFileInfo implements fs.FileInfo for remotestore.
type remoteFileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	isDir bool
}

func (fi *remoteFileInfo) Name() string      { return fi.name }
func (fi *remoteFileInfo) Size() int64        { return fi.size }
func (fi *remoteFileInfo) ModTime() time.Time { return store.FakeStaticFileTime }
func (fi *remoteFileInfo) IsDir() bool        { return fi.isDir }
func (fi *remoteFileInfo) Sys() any           { return nil }
func (fi *remoteFileInfo) Mode() fs.FileMode {
	if fi.isDir {
		return fi.mode | fs.ModeDir
	}
	return fi.mode
}

// Ensure Store implements store.Store at compile time.
var _ store.Store = (*Store)(nil)
