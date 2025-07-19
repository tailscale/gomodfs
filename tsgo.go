package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tailscale/gomodfs/store"
)

// tsGoRoot is Tailscale's ~/.cache/tsgo directory.
// It pretends to the GOOS as given by the "goos" and "goarch" fields.
//
// It can contain two types of entries:
//  1. ${git-hash}.extracted empty file
//  2. ${git-hash}/ directory of contents
//
// Where git-hash is the hash of a github.com/tailscale/go
// commit which becomes its release filename like
// https://github.com/tailscale/go/releases/download/build-1cd3bf1a6eaf559aa8c00e749289559c884cef09/linux-amd64.tar.gz
// which is downloaded and git-tree-ified to back
// the directory in (2) above.
type tsgoRoot struct {
	fs.Inode
	fs *FS

	goos   string // "linux", "darwin", etc.
	goarch string // "amd64", "arm64", etc.
}

var _ = (fs.NodeLookuper)((*tsgoRoot)(nil))

var tsgoRootLookupRx = regexp.MustCompile(`^([a-f0-9]{40})(\.extracted)?$`)

func (n *tsgoRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	m := tsgoRootLookupRx.FindStringSubmatch(name)
	if m == nil {
		return nil, syscall.ENOENT
	}

	hash, wantExtractedFile := m[1], m[2] != ""

	mv, root, err := n.fs.getTailscaleGoRoot(ctx, n.goos, n.goarch, hash)
	if err != nil {
		log.Printf("Failed to get tailscale go root for %s/%s/%s: %v", n.goos, n.goarch, hash, err)
		return nil, syscall.EIO
	}

	if wantExtractedFile {
		// If it's a file, it must be an empty file.
		in := n.NewInode(ctx, &memFile{
			contents: []byte{},
			mode:     0644,
		}, fs.StableAttr{
			Mode: fuse.S_IFREG | 0644,
		})
		return in, 0
	}

	return n.NewInode(ctx, &pathUnderZipRoot{
		fs:   n.fs,
		mv:   mv,
		root: root,
		mode: os.ModeDir,
	}, fs.StableAttr{
		Mode: fuse.S_IFDIR | 0755,
	}), 0

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
	log.Printf("Downloading %q", urlStr)
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
