// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"context"
	"crypto/sha256"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"net"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
	nfsfile "github.com/willscott/go-nfs/file"
)

// NFSHandler retains checkouts and their handles until the last
// [NFSHandler.Acquire] caller releases them. Handles are stable SHA-256 hashes
// of the repository name, commit, and path, but their reverse mappings are not
// persisted or reconstructed. After release or restart, a handle is stale until
// the checkout is acquired and ToHandle registers that path again.
type NFSHandler struct {
	repos *Manager         // repos opens the immutable checkout behind each acquired export.
	owner nfsfile.FileInfo // owner is the common NFS ownership and link-count metadata.

	// mu protects exports, handles, and the mutable fields of each repoExport.
	mu      sync.Mutex
	exports map[string]*repoExport               // maps <repo>/<commit> mount paths to acquired checkouts.
	handles map[[sha256.Size]byte]repoPathHandle // maps NFS handles to their corresponding repo.
}

// repoExport holds the shared state for one acquired repository checkout.
type repoExport struct {
	co *checkout

	// refs counts active Acquire leases for the checkout.
	refs int

	// paths maps a path within a repo to its NFS handle. It's tracked so
	// we can garbage collect unused handles from NFSHandler once the export is
	// no longer referenced.
	paths map[string][sha256.Size]byte
}

// repoPathHandle tracks a path within a specific exported repo.
type repoPathHandle struct {
	export *repoExport // export is the acquired checkout that owns the handle.
	path   string      // path is relative to the root of export.
}

var _ nfs.ReadHandler = (*NFSHandler)(nil)

// NewNFSHandler returns a read-only NFS handler for checkouts managed by repos.
func NewNFSHandler(repos *Manager, uid, gid uint32) (*NFSHandler, error) {
	if err := repos.validate(); err != nil {
		return nil, err
	}
	return &NFSHandler{
		repos: repos,
		owner: nfsfile.FileInfo{
			UID:   uid,
			GID:   gid,
			Nlink: 1,
		},
		exports: map[string]*repoExport{},
		handles: map[[sha256.Size]byte]repoPathHandle{},
	}, nil
}

// Acquire makes /repos/<repo>/<commit> mountable until release is called.
// Call once per guest before mounting, and release after that guest stops.
// Release is idempotent; the last release discards the checkout's handle
// mappings and frees its in-memory state. The on-disk Git cache is retained.
func (h *NFSHandler) Acquire(ctx context.Context, repo, commit string) (release func(), err error) {
	key := repo + "/" + commit
	h.mu.Lock()
	e := h.exports[key]
	if e != nil {
		e.refs++
	}
	h.mu.Unlock()
	if e == nil {
		co, err := h.repos.checkout(ctx, repo, commit)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		e = h.exports[key]
		if e == nil {
			e = &repoExport{
				co:    co,
				paths: map[string][sha256.Size]byte{},
			}
			h.exports[key] = e
		}
		e.refs++
		h.mu.Unlock()
	}
	return sync.OnceFunc(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		e.refs--
		if e.refs == 0 {
			delete(h.exports, key)
			for _, id := range e.paths {
				delete(h.handles, id)
			}
		}
	}), nil
}

// Mount resolves an acquired /repos/<repo>/<commit> export.
func (h *NFSHandler) Mount(_ context.Context, _ net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	key, ok := strings.CutPrefix(strings.Trim(string(req.Dirpath), "/"), "repos/")
	if !ok {
		return nfs.MountStatusErrNoEnt, nil, nil
	}
	h.mu.Lock()
	e := h.exports[key]
	h.mu.Unlock()
	if e == nil {
		return nfs.MountStatusErrNoEnt, nil, nil
	}
	return nfs.MountStatusOk, h.filesystem(e), []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

func (h *NFSHandler) filesystem(e *repoExport) checkoutBilly {
	return checkoutBilly{
		export: e,
		owner:  h.owner,
	}
}

// Change returns nil because repository checkouts are read-only.
func (h *NFSHandler) Change(billy.Filesystem) billy.Change { return nil }

// FSStat leaves filesystem capacity fields at their default values.
func (h *NFSHandler) FSStat(context.Context, billy.Filesystem, *nfs.FSStat) error {
	return nil
}

// InvalidateHandle is a no-op because handle mappings live until their export is released.
func (h *NFSHandler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

// HandleLimit reports that the handler imposes no practical handle limit.
func (h *NFSHandler) HandleLimit() int { return math.MaxInt }

// ToHandle returns a stable handle for segs and registers its reverse mapping
// for the lifetime of the acquired export.
func (h *NFSHandler) ToHandle(f billy.Filesystem, segs []string) []byte {
	// go-nfs calls ToHandle even after a rejected mount with a nil filesystem.
	b, ok := f.(checkoutBilly)
	if !ok {
		return make([]byte, sha256.Size)
	}
	p := strings.Join(segs, "/")
	h.mu.Lock()
	defer h.mu.Unlock()
	if b.export.refs == 0 {
		return make([]byte, sha256.Size)
	}
	id, ok := b.export.paths[p]
	if !ok {
		// NUL separators make the encoding unambiguous: repository names and
		// commit hashes cannot contain NULs.
		co := b.export.co
		id = sha256.Sum256([]byte(co.repo.name + "\x00" + co.commit + "\x00" + p))
		b.export.paths[p] = id
		h.handles[id] = repoPathHandle{
			export: b.export,
			path:   p,
		}
	}
	return slices.Clone(id[:])
}

func staleErr(err error) error {
	return &nfs.NFSStatusError{
		NFSStatus:  nfs.NFSStatusStale,
		WrappedErr: err,
	}
}

// FromHandle resolves an opaque handle to its live checkout and path.
func (h *NFSHandler) FromHandle(handle []byte) (billy.Filesystem, []string, error) {
	if len(handle) != sha256.Size {
		return nil, nil, staleErr(errors.New("invalid repository handle length"))
	}
	id := [sha256.Size]byte(handle)
	h.mu.Lock()
	entry, ok := h.handles[id]
	h.mu.Unlock()
	if !ok {
		return nil, nil, staleErr(os.ErrNotExist)
	}
	var segs []string
	if entry.path != "" {
		segs = strings.Split(entry.path, "/")
	}
	return h.filesystem(entry.export), segs, nil
}

// OnNFSRead reads file contents directly from the checkout backing handle.
func (h *NFSHandler) OnNFSRead(ctx context.Context, handle []byte, offset uint64, count uint32) (*nfs.NFSReadResult, error) {
	f, segs, err := h.FromHandle(handle)
	if err != nil {
		return nil, err
	}
	b := f.(checkoutBilly)
	p := strings.Join(segs, "/")
	data, err := b.export.co.readFile(ctx, p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &nfs.NFSStatusError{
				NFSStatus:  nfs.NFSStatusNoEnt,
				WrappedErr: err,
			}
		}
		return nil, err
	}
	fi, err := b.Lstat(p)
	if err != nil {
		return nil, err
	}
	size := uint64(len(data))
	start := min(size, offset)
	end := start + min(size-start, uint64(count), uint64(nfs.MaxRead))
	return &nfs.NFSReadResult{
		Data: data[start:end],
		EOF:  end == size,
		Attr: nfs.ToFileAttribute(fi, p),
	}, nil
}

// checkoutFileInfo combines checkout metadata with attributes used by go-nfs.
type checkoutFileInfo struct {
	fileInfo os.FileInfo      // fileInfo is the metadata reported by the underlying checkout.
	nfsAttrs nfsfile.FileInfo // nfsAttrs supplies ownership and file identity through Sys.
}

var _ os.FileInfo = (*checkoutFileInfo)(nil)

func (fi *checkoutFileInfo) Name() string       { return fi.fileInfo.Name() }
func (fi *checkoutFileInfo) Size() int64        { return fi.fileInfo.Size() }
func (fi *checkoutFileInfo) Mode() os.FileMode  { return fi.fileInfo.Mode() }
func (fi *checkoutFileInfo) ModTime() time.Time { return fi.fileInfo.ModTime() }
func (fi *checkoutFileInfo) IsDir() bool        { return fi.fileInfo.IsDir() }
func (fi *checkoutFileInfo) Sys() any           { return fi.nfsAttrs }

// checkoutBilly adapts a live immutable checkout to the metadata-oriented
// subset of billy.Filesystem used by go-nfs. File data bypasses this adapter
// and is served by NFSHandler.OnNFSRead.
type checkoutBilly struct {
	export *repoExport      // export identifies the live checkout and ties the adapter to its lease.
	owner  nfsfile.FileInfo // owner is the NFS attribute template applied to files in the checkout.
}

var _ billy.Filesystem = checkoutBilly{}

func (checkoutBilly) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}
func (checkoutBilly) Join(e ...string) string                 { return path.Join(e...) }
func (checkoutBilly) Root() string                            { return "/" }
func (checkoutBilly) Chroot(string) (billy.Filesystem, error) { return nil, errors.ErrUnsupported }
func (checkoutBilly) Create(string) (billy.File, error)       { return nil, errRepositoryReadonly }

// File reads are handled by OnNFSRead instead, which is defined in the
// [nfs.ReadHandler] interface.
func (checkoutBilly) Open(string) (billy.File, error) { return nil, errors.ErrUnsupported }
func (checkoutBilly) OpenFile(string, int, os.FileMode) (billy.File, error) {
	return nil, errors.ErrUnsupported
}

var errRepositoryReadonly = errors.New("repository checkout is read-only")

func (checkoutBilly) Rename(string, string) error                 { return errRepositoryReadonly }
func (checkoutBilly) Remove(string) error                         { return errRepositoryReadonly }
func (checkoutBilly) TempFile(string, string) (billy.File, error) { return nil, errRepositoryReadonly }
func (checkoutBilly) MkdirAll(string, os.FileMode) error          { return errRepositoryReadonly }
func (checkoutBilly) Symlink(string, string) error                { return errRepositoryReadonly }

func (b checkoutBilly) own(fi os.FileInfo, p string) os.FileInfo {
	o := b.owner
	h := fnv.New64()
	io.WriteString(h, b.export.co.repo.name)
	io.WriteString(h, b.export.co.commit)
	io.WriteString(h, p)
	o.Fileid = h.Sum64()
	return &checkoutFileInfo{
		fileInfo: fi,
		nfsAttrs: o,
	}
}
func (b checkoutBilly) Stat(p string) (os.FileInfo, error) { return b.Lstat(p) }
func (b checkoutBilly) Lstat(p string) (os.FileInfo, error) {
	fi, err := b.export.co.stat(context.Background(), p)
	if err != nil {
		return nil, err
	}
	return b.own(fi, p), nil
}
func (b checkoutBilly) Readlink(p string) (string, error) {
	return b.export.co.readlink(context.Background(), p)
}
func (b checkoutBilly) ReadDir(p string) ([]os.FileInfo, error) {
	ents, err := b.export.co.readDir(context.Background(), p)
	if err != nil {
		return nil, err
	}
	ret := make([]os.FileInfo, 0, len(ents))
	for _, e := range ents {
		child := e.name
		if p != "" {
			child = p + "/" + child
		}
		fi, err := b.export.co.stat(context.Background(), child)
		if err != nil {
			return nil, err
		}
		ret = append(ret, b.own(fi, child))
	}
	slices.SortFunc(ret, func(a, b os.FileInfo) int { return strings.Compare(a.Name(), b.Name()) })
	return ret, nil
}
