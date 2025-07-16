package main

import (
	"context"
	"regexp"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
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
	conf *FS

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

	hash, wantFile := m[1], m[2] != ""

	treeHash, err := n.conf.Git.GetTailscaleGo(n.goos, n.goarch, hash)
	if err != nil {
		return nil, syscall.EIO
	}
	if wantFile {
		// If it's a file, it must be an empty file.
		in := n.NewInode(ctx, &memFile{
			contents: []byte{},
			mode:     0644,
		}, fs.StableAttr{
			Mode: fuse.S_IFREG | 0644,
		})
		return in, 0
	}
	// If it's a directory, it must be a directory with the contents of the git
	// tree of the tsgo commit.
	in := n.NewInode(ctx, &treeNode{
		fs:   n.conf,
		tree: treeHash,
	}, fs.StableAttr{
		Mode: fuse.S_IFDIR | 0755,
	})
	return in, 0
}
