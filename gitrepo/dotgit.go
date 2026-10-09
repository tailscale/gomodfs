// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"

	"github.com/tailscale/nfsv4"
	"github.com/tailscale/nfsv4/nodefs"
)

// newDotGit returns a read-only .git directory for co. It contains the
// minimum that Git needs to open a shallow repository: HEAD, an index, and
// the loose objects of the commit. The objects directory contains only the
// objects of this commit, so a view never shows other commits.
func newDotGit(o owner, co *Checkout) nodefs.Node {
	file := func(data []byte) nodefs.Node {
		return memFile{
			owner: o,
			data:  data,
		}
	}
	return &staticDir{
		owner: o,
		entries: []nodefs.DirEntry{
			{
				Name: "HEAD",
				Node: file([]byte(co.commit + "\n")),
			},
			{
				Name: "config",
				Node: file([]byte(checkoutConfig)),
			},
			{
				Name: "index",
				Node: file(co.objects.index),
			},
			{
				Name: "objects",
				Node: &objectsDir{
					owner: o,
					co:    co,
				},
			},
			{
				Name: "refs",
				Node: &staticDir{
					owner: o,
					entries: []nodefs.DirEntry{
						{
							Name: "heads",
							Node: &staticDir{
								owner: o,
							},
						},
					},
				},
			},
			{
				// The parents of the commit are not available.
				Name: "shallow",
				Node: file([]byte(co.commit + "\n")),
			},
		},
	}
}

// checkoutConfig is .git/config. It tells Git not to examine the files of the
// checkout, which cannot change:
//
//   - Index preloading does not obey the assume-unchanged flag (see
//     [encodeIndex]), and would stat every file.
//   - The checkout is read-only, so it cannot contain untracked files, and
//     git status does not need to read every directory to look for them.
const checkoutConfig = `[core]
	repositoryformatversion = 0
	bare = false
	preloadIndex = false
[status]
	showUntrackedFiles = no
`

// objects is the set of objects of a commit, and its index file.
type objects struct {
	types map[string]string   // Object ID to type.
	dirs  map[string][]string // Loose object directory ("ab") to file names.
	index []byte
}

// newObjects returns the objects and index of commit, whose root tree is
// tree. Entries are all trees and files of the commit.
func newObjects(commit, tree string, entries []treeEntry) *objects {
	o := &objects{
		types: map[string]string{
			commit: "commit",
			tree:   "tree",
		},
		dirs:  map[string][]string{},
		index: encodeIndex(tree, entries),
	}
	for _, e := range entries {
		switch e.mode & 0o170000 {
		case 0o040000:
			o.types[e.id] = "tree"
		case 0o160000: // A submodule commit is not in this repository.
		default:
			o.types[e.id] = "blob"
		}
	}
	for _, id := range slices.Sorted(maps.Keys(o.types)) {
		o.dirs[id[:2]] = append(o.dirs[id[:2]], id[2:])
	}
	return o
}

// cacheTree is a node of the cache tree index extension.
type cacheTree struct {
	name    string // Path component; empty for the root.
	id      string
	entries int // Number of index entries below the tree.
	subs    []*cacheTree
}

// encodeIndex returns a version 2 index file for the tree root. Entries are
// the result of a recursive git ls-tree -t, so trees come before their
// entries and the rest is in path order.
//
// The checkout cannot change, so the index tells Git not to examine it. The
// index has no stat data, and every entry has the assume-unchanged flag, so
// Git does not compare the files with the index. The cache tree extension
// gives the ID of every tree, so Git does not read tree objects to compare
// the index with HEAD.
func encodeIndex(root string, entries []treeEntry) []byte {
	trees := map[string]*cacheTree{
		".": {
			id: root,
		},
	}
	var files []treeEntry
	for _, e := range entries {
		dir := path.Dir(e.name)
		if e.mode&0o170000 == 0o040000 {
			t := &cacheTree{
				name: path.Base(e.name),
				id:   e.id,
			}
			trees[e.name] = t
			trees[dir].subs = append(trees[dir].subs, t)
			continue
		}
		files = append(files, e)
		for ; dir != "."; dir = path.Dir(dir) {
			trees[dir].entries++
		}
		trees["."].entries++
	}

	be := binary.BigEndian
	b := []byte("DIRC")
	b = be.AppendUint32(b, 2)
	b = be.AppendUint32(b, uint32(len(files)))
	for _, e := range files {
		start := len(b)
		b = append(b, make([]byte, 24)...) // ctime, mtime, dev, ino
		b = be.AppendUint32(b, e.mode)
		b = append(b, make([]byte, 8)...) // uid, gid
		b = be.AppendUint32(b, uint32(e.size))
		id, _ := hex.DecodeString(e.id)
		b = append(b, id...)
		const assumeValid = 0x8000
		b = be.AppendUint16(b, assumeValid|uint16(min(len(e.name), 0xfff)))
		b = append(b, e.name...)
		// One to eight NUL bytes end the entry at a multiple of 8 bytes.
		b = append(b, make([]byte, 8-(len(b)-start)%8)...)
	}

	// The extension lists the trees in preorder. Each tree is
	// "<name>\x00<entries> <subtrees>\n<binary ID>".
	var ext []byte
	var appendTree func(t *cacheTree)
	appendTree = func(t *cacheTree) {
		ext = fmt.Appendf(ext, "%s\x00%d %d\n", t.name, t.entries, len(t.subs))
		id, _ := hex.DecodeString(t.id)
		ext = append(ext, id...)
		for _, sub := range t.subs {
			appendTree(sub)
		}
	}
	appendTree(trees["."])
	b = append(b, "TREE"...)
	b = be.AppendUint32(b, uint32(len(ext)))
	b = append(b, ext...)

	sum := sha1.Sum(b)
	return append(b, sum[:]...)
}

// memFile is a file with fixed contents.
type memFile struct {
	owner
	data []byte
}

func (f memFile) Attr(*nfsv4.Request) (*nfsv4.Attrs, error) {
	return f.attrs(nfsv4.TypeReg, 0o444, int64(len(f.data))), nil
}

func (f memFile) ReadAt(_ *nfsv4.Request, p []byte, off int64) (int, error) {
	return readAt(f.data, p, off)
}

// objectsDir is .git/objects if prefix is empty, and .git/objects/<prefix>
// otherwise.
type objectsDir struct {
	owner
	co     *Checkout
	prefix string
}

func (d *objectsDir) Attr(*nfsv4.Request) (*nfsv4.Attrs, error) {
	return d.attrs(nfsv4.TypeDir, 0o555, 0), nil
}

func (d *objectsDir) ReadDir(*nfsv4.Request) ([]nodefs.DirEntry, error) {
	o := d.co.objects
	names := o.dirs[d.prefix]
	if d.prefix == "" {
		names = slices.Sorted(maps.Keys(o.dirs))
	}
	// The entries have no nodes, so that listing a directory does not
	// compress all of its objects.
	entries := make([]nodefs.DirEntry, len(names))
	for i, name := range names {
		entries[i].Name = name
	}
	return entries, nil
}

func (d *objectsDir) Lookup(_ *nfsv4.Request, name string) (nodefs.Node, error) {
	o := d.co.objects
	if d.prefix == "" {
		if _, ok := o.dirs[name]; !ok {
			return nil, fs.ErrNotExist
		}
		return &objectsDir{
			owner:  d.owner,
			co:     d.co,
			prefix: name,
		}, nil
	}
	id := d.prefix + name
	typ, ok := o.types[id]
	if !ok {
		return nil, fs.ErrNotExist
	}
	data, err := d.co.repo.readObject(id)
	if err != nil {
		return nil, err
	}
	// A loose object is the zlib-compressed object with a header.
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	fmt.Fprintf(zw, "%s %d\x00", typ, len(data))
	zw.Write(data)
	zw.Close()
	return memFile{
		owner: d.owner,
		data:  buf.Bytes(),
	}, nil
}
