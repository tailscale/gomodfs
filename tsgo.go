// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tailscale/gomodfs/store"
)

var (
	tsGoGeese    = []string{"linux", "darwin", "windows"}
	tsGoGoarches = []string{"amd64", "arm64"}
)

const wkTSGoExtracted = "tsgo.extracted"

func validTSGoOSARCH(goos, goarch string) bool {
	// Keep this in sync with above.
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return false
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return false
	}
	return true
}

// tsgoFile implements store.PutFile for a file in the tsgo root for use when
// we're filling the Store cache from a tarball downloaded from the Tailscale Go
// releases.
type tsgoFile struct {
	path    string
	mode    os.FileMode
	content []byte
}

func (f tsgoFile) Path() string                 { return f.path }
func (f tsgoFile) Size() int64                  { return int64(len(f.content)) }
func (f tsgoFile) Open() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.content)), nil }
func (f tsgoFile) Mode() os.FileMode            { return f.mode }

func isTSGoModule(mv store.ModuleVersion) (ret tsgoTriple, ok bool) {
	if mv.Module != "github.com/tailscale/go" {
		return
	}
	// mv.Version is "tsgo-GOOS-GOARCH-HASH"
	rest, ok := strings.CutPrefix(mv.Version, "tsgo-")
	if !ok {
		return
	}
	// rest is "GOOS-GOARCH-HASH"
	goos, rest, ok := strings.Cut(rest, "-")
	if !ok {
		return
	}
	goarch, hash, ok := strings.Cut(rest, "-")
	if !ok {
		return
	}
	return tsgoTriple{OS: goos, Arch: goarch, Hash: hash}, true
}

func (fs *FS) getTailscaleGoRoot(ctx context.Context, goos, goarch, commitHash string) (store.ModuleVersion, store.ModHandle, error) {

	mv := store.ModuleVersion{
		Module:  "github.com/tailscale/go",
		Version: "tsgo-" + goos + "-" + goarch + "-" + commitHash,
	}
	h, err := fs.Store.GetZipRoot(ctx, mv)
	if err == nil {
		return mv, h, nil
	}
	if !errors.Is(err, store.ErrCacheMiss) {
		return mv, nil, err
	}

	// Cache fill.

	var data store.PutModuleData

	// If it doesn't exist, download the tarball.
	urlStr := fmt.Sprintf("https://github.com/tailscale/go/releases/download/build-%s/%s-%s.tar.gz", commitHash, goos, goarch)
	fs.netLogf("starting download of %q ...", urlStr)
	tgz, err := fs.netSlurp(ctx, urlStr)
	if err != nil {
		return mv, "", fmt.Errorf("failed to download %q: %w", urlStr, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return mv, "", fmt.Errorf("failed to gunzip tarball: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break // end of tar
		}
		if err != nil {
			return mv, "", fmt.Errorf("failed to read tar header: %w", err)
		}
		if h.FileInfo().IsDir() {
			continue
		}
		all, err := io.ReadAll(tr)
		if err != nil {
			return mv, "", fmt.Errorf("failed to read tar file %q: %w", h.Name, err)
		}
		data.Files = append(data.Files, tsgoFile{
			path:    strings.TrimPrefix(h.Name, "go/"),
			mode:    h.FileInfo().Mode(),
			content: all,
		})
	}

	root, err := fs.Store.PutModule(ctx, mv, data)
	return mv, root, err
}
