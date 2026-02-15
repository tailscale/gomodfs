// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package remotestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/pierrec/lz4/v4"
	"github.com/tailscale/gomodfs/store"
)

// FSBackend is the interface the handler needs from the gomodfs.FS type.
// This avoids a circular import between gomodfs and remotestore.
type FSBackend interface {
	// GetZipRootOrDownload returns a ModHandle, downloading from the proxy if needed.
	GetZipRootOrDownload(ctx context.Context, mv store.ModuleVersion) (store.ModHandle, error)

	// GetMetaFileByExt returns metadata file contents for "mod", "info", or "ziphash".
	GetMetaFileByExt(ctx context.Context, mv store.ModuleVersion, ext string) ([]byte, error)

	// GetStore returns the underlying store.Store.
	GetStore() store.Store
}

// Handler returns an http.Handler that serves the remotestore API
// backed by the given FSBackend (typically a *gomodfs.FS).
func Handler(backend FSBackend) http.Handler {
	h := &handler{
		backend: backend,
		store:   backend.GetStore(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/modmap/", h.handleModMap)
	mux.HandleFunc("GET /api/v1/file/", h.handleFile)
	mux.HandleFunc("GET /api/v1/meta/", h.handleMeta)
	return mux
}

type handler struct {
	backend FSBackend
	store   store.Store

	mu       sync.Mutex
	modmapLZ map[store.ModuleVersion][]byte // cached lz4-compressed modmap JSON
}

func (h *handler) handleModMap(w http.ResponseWriter, r *http.Request) {
	mv, _, err := parseMVFromPath(r.URL.Path, modmapPrefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !acceptsLZ4(r) {
		http.Error(w, "Accept-Encoding must include lz4", http.StatusNotAcceptable)
		return
	}

	// Check cache for pre-compressed response.
	h.mu.Lock()
	cached := h.modmapLZ[mv]
	h.mu.Unlock()
	if cached != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "lz4")
		w.Write(cached)
		return
	}

	ctx := r.Context()

	// Get or download the zip root.
	mh, err := h.backend.GetZipRootOrDownload(ctx, mv)
	if err != nil {
		if errors.Is(err, store.ErrCacheMiss) {
			http.Error(w, "module not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Build the file map by recursively walking the store.
	fmap := make(ModVersionMap)
	if err := walkFiles(ctx, h.store, mh, "", fmap); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// JSON-encode.
	jsonData, err := json.Marshal(fmap)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// LZ4-compress.
	var buf bytes.Buffer
	lzw := lz4.NewWriter(&buf)
	if _, err := lzw.Write(jsonData); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := lzw.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	compressed := buf.Bytes()

	// Cache the compressed bytes.
	h.mu.Lock()
	if h.modmapLZ == nil {
		h.modmapLZ = make(map[store.ModuleVersion][]byte)
	}
	h.modmapLZ[mv] = compressed
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "lz4")
	w.Write(compressed)
}

func (h *handler) handleFile(w http.ResponseWriter, r *http.Request) {
	mv, _, err := parseMVFromPath(r.URL.Path, filePrefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "missing path query parameter", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	mh, err := h.backend.GetZipRootOrDownload(ctx, mv)
	if err != nil {
		if errors.Is(err, store.ErrCacheMiss) {
			http.Error(w, "module not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := h.store.GetFile(ctx, mh, filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

func (h *handler) handleMeta(w http.ResponseWriter, r *http.Request) {
	mv, ext, err := parseMVFromPath(r.URL.Path, metaPrefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	data, err := h.backend.GetMetaFileByExt(ctx, mv, ext)
	if err != nil {
		if errors.Is(err, store.ErrCacheMiss) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

// walkFiles recursively walks a module's file tree and populates fmap.
func walkFiles(ctx context.Context, s store.Store, mh store.ModHandle, dir string, fmap ModVersionMap) error {
	ents, err := s.Readdir(ctx, mh, dir)
	if err != nil {
		return err
	}
	for _, ent := range ents {
		name := ent.Name
		if dir != "" {
			name = dir + "/" + name
		}
		if ent.Mode.IsDir() {
			if err := walkFiles(ctx, s, mh, name, fmap); err != nil {
				return err
			}
		} else {
			fmap[name] = FileMeta{Size: ent.Size, Mode: ent.Mode}
		}
	}
	return nil
}

// acceptsLZ4 checks if the request's Accept-Encoding header includes "lz4".
func acceptsLZ4(r *http.Request) bool {
	for _, v := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		encoding, _, _ := strings.Cut(strings.TrimSpace(v), ";")
		if strings.EqualFold(strings.TrimSpace(encoding), "lz4") {
			return true
		}
	}
	return false
}
