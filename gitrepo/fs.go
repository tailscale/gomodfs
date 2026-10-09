// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/tailscale/nfsv4"
	"github.com/tailscale/nfsv4/nodefs"
)

var staticTime = time.Date(2009, 11, 12, 13, 14, 15, 0, time.UTC)

var (
	_ nodefs.Dir     = (*staticDir)(nil)
	_ nodefs.Dir     = (*gitDir)(nil)
	_ nodefs.File    = gitFile{}
	_ nodefs.Symlink = gitLink{}
)

// FSOptions are the options for [NewFS].
type FSOptions struct {
	// UID and GID are the owner of all files.
	UID, GID uint32

	// Checkouts are the checkouts to serve. Each must be of a different
	// repository.
	Checkouts []*Checkout
}

// NewFS returns a read-only filesystem that serves each checkout of opts at
// /repos/<owner>/<repo>. Serve it with an [nfsv4.Server].
//
// Each checkout has a read-only .git directory with the loose objects of the
// commit, so that read-only Git commands work.
func NewFS(opts FSOptions) (*nodefs.FS, error) {
	o := owner{
		uid: opts.UID,
		gid: opts.GID,
	}
	repos := &staticDir{
		owner: o,
	}
	owners := map[string]*staticDir{}
	for _, co := range opts.Checkouts {
		ownerName, repoName, _ := strings.Cut(string(co.repo.name), "/")
		od := owners[ownerName]
		if od == nil {
			od = &staticDir{
				owner: o,
			}
			owners[ownerName] = od
			repos.entries = append(repos.entries, nodefs.DirEntry{
				Name: ownerName,
				Node: od,
			})
		}
		if _, err := lookup(od.entries, repoName); err == nil {
			return nil, fmt.Errorf("gitrepo: more than one checkout of %s", co.repo.name)
		}
		od.entries = append(od.entries, nodefs.DirEntry{
			Name: repoName,
			Node: &gitDir{
				owner:  o,
				co:     co,
				id:     co.tree,
				dotGit: newDotGit(o, co),
			},
		})
	}
	root := &staticDir{
		owner: o,
		entries: []nodefs.DirEntry{{
			Name: "repos",
			Node: repos,
		}},
	}
	// All contents are immutable, so clients can always cache them.
	return nodefs.New(root, &nodefs.Options{
		DefaultCache: nfsv4.Delegation{
			Grant: true,
		},
	}), nil
}

type owner struct {
	uid, gid uint32
}

func (o owner) attrs(typ nfsv4.FileType, mode uint32, size int64) *nfsv4.Attrs {
	return &nfsv4.Attrs{
		Type:    typ,
		Change:  1, // The contents never change.
		Size:    uint64(size),
		Mode:    mode,
		UID:     o.uid,
		GID:     o.gid,
		ModTime: staticTime,
	}
}

// staticDir is a directory with fixed entries. It is used for the layout
// directories above each checkout and for submodules, which are empty.
type staticDir struct {
	owner
	entries []nodefs.DirEntry
}

func (d *staticDir) Attr(*nfsv4.Request) (*nfsv4.Attrs, error) {
	return d.attrs(nfsv4.TypeDir, 0o555, 0), nil
}

func (d *staticDir) ReadDir(*nfsv4.Request) ([]nodefs.DirEntry, error) {
	return d.entries, nil
}

func (d *staticDir) Lookup(_ *nfsv4.Request, name string) (nodefs.Node, error) {
	return lookup(d.entries, name)
}

func lookup(entries []nodefs.DirEntry, name string) (nodefs.Node, error) {
	for _, e := range entries {
		if e.Name == name {
			return e.Node, nil
		}
	}
	return nil, fs.ErrNotExist
}

// gitDir is a Git tree.
type gitDir struct {
	owner
	co     *Checkout
	id     string
	dotGit nodefs.Node // The synthetic .git directory; nil except at the root.
}

func (d *gitDir) Attr(*nfsv4.Request) (*nfsv4.Attrs, error) {
	return d.attrs(nfsv4.TypeDir, 0o555, 0), nil
}

func (d *gitDir) Lookup(_ *nfsv4.Request, name string) (nodefs.Node, error) {
	if d.dotGit != nil && name == ".git" {
		return d.dotGit, nil
	}
	for _, te := range d.co.dirs[d.id] {
		if te.name == name {
			return d.node(te), nil
		}
	}
	return nil, fs.ErrNotExist
}

func (d *gitDir) ReadDir(*nfsv4.Request) ([]nodefs.DirEntry, error) {
	tes := d.co.dirs[d.id]
	entries := make([]nodefs.DirEntry, 0, len(tes))
	for _, te := range tes {
		entries = append(entries, nodefs.DirEntry{
			Name: te.name,
			Node: d.node(te),
		})
	}
	if d.dotGit != nil {
		entries = append(entries, nodefs.DirEntry{
			Name: ".git",
			Node: d.dotGit,
		})
	}
	return entries, nil
}

func (d *gitDir) node(te treeEntry) nodefs.Node {
	switch te.mode & 0o170000 {
	case 0o040000:
		return &gitDir{
			owner: d.owner,
			co:    d.co,
			id:    te.id,
		}
	case 0o160000: // A submodule. Like git, show an empty directory.
		return &staticDir{
			owner: d.owner,
		}
	case 0o120000:
		return gitLink{
			gitFile: gitFile{
				owner: d.owner,
				repo:  d.co.repo,
				te:    te,
			},
		}
	}
	return gitFile{
		owner: d.owner,
		repo:  d.co.repo,
		te:    te,
	}
}

// gitFile is a Git blob.
type gitFile struct {
	owner
	repo *repository
	te   treeEntry
}

func (f gitFile) Attr(*nfsv4.Request) (*nfsv4.Attrs, error) {
	mode := uint32(0o444)
	if f.te.mode&0o100 != 0 {
		mode = 0o555
	}
	return f.attrs(nfsv4.TypeReg, mode, f.te.size), nil
}

func (f gitFile) ReadAt(_ *nfsv4.Request, p []byte, off int64) (int, error) {
	b, err := f.repo.readObject(f.te.id)
	if err != nil {
		return 0, err
	}
	return readAt(b, p, off)
}

func readAt(b, p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if off+int64(n) == int64(len(b)) {
		return n, io.EOF
	}
	return n, nil
}

// gitLink is a symbolic link. Its blob contains the target.
type gitLink struct {
	gitFile
}

func (l gitLink) Attr(*nfsv4.Request) (*nfsv4.Attrs, error) {
	return l.attrs(nfsv4.TypeSymlink, 0o777, l.te.size), nil
}

func (l gitLink) Readlink(*nfsv4.Request) (string, error) {
	b, err := l.repo.readObject(l.te.id)
	return string(b), err
}
