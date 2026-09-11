// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package nfsexports shares an NFS endpoint between the module cache and Git
// checkouts so rpcbind-dependent clients can mount both from one server.
package nfsexports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"net"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/tailscale/gomodfs"
	"github.com/tailscale/gomodfs/gitrepo"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
)

// New creates a [nfs.ReadHandler] that routes between go mod and git repo
// exports based on a handle prefix. Both arguments must be non-nil.
func New(mfs *gomodfs.FS, gfs *gitrepo.NFSHandler) nfs.ReadHandler {
	return &handler{
		mfs: mfs.NFSHandler().(nfs.ReadHandler),
		gfs: gfs,
	}
}

type handler struct {
	mfs nfs.ReadHandler
	gfs *gitrepo.NFSHandler
}

var _ nfs.ReadHandler = (*handler)(nil)

type handleKind byte

const (
	moduleHandleKind handleKind = 0
	repoHandleKind   handleKind = 1
	handleMagic                 = "GOMODFS"
	handleVersion    byte       = 1
)

var errStaleHandle = &nfs.NFSStatusError{
	NFSStatus:  nfs.NFSStatusStale,
	WrappedErr: errors.New("invalid export handle"),
}

// decodeHandle determines whether the requested handle belongs to a go mod or
// git repo export. Git repo handles have a magic header and version/kind bytes
// that precede the actual handle payload which [gitrepo.NFSHandler] understands.
func decodeHandle(handle []byte) (handleKind, []byte, error) {
	// The original go module handles are composed of 2 SHA256 hashes occupying
	// all 64 bytes allowed by NFSv3. Handles for other exports must be shorter
	// to distinguish them from go mod handles. The short not-exist sentinel is
	// also part of the original wire format.
	if len(handle) == 64 || bytes.Equal(handle, []byte("\x00\xFF\x00Nope")) {
		return moduleHandleKind, handle, nil
	}
	const headerLen = len(handleMagic) + 2
	if len(handle) >= 64 || len(handle) < headerLen+1 || !bytes.HasPrefix(handle, []byte(handleMagic)) {
		return 0, nil, errStaleHandle
	}
	if handle[len(handleMagic)] != handleVersion {
		return 0, nil, errStaleHandle
	}
	kind := handleKind(handle[len(handleMagic)+1])
	payload := handle[headerLen:]
	switch kind {
	case repoHandleKind:
		if len(payload) != sha256.Size {
			return 0, nil, errStaleHandle
		}
	default:
		return 0, nil, errStaleHandle
	}
	return kind, payload, nil
}

type repoFS struct{ billy.Filesystem }

func (repoFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}

// Mount implements [nfs.ReadHandler].
func (h *handler) Mount(ctx context.Context, conn net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	p := strings.Trim(string(req.Dirpath), "/")
	if p == "repos" || strings.HasPrefix(p, "repos/") {
		status, f, flavors := h.gfs.Mount(ctx, conn, req)
		if status == nfs.MountStatusOk {
			f = repoFS{
				Filesystem: f,
			}
		}
		return status, f, flavors
	}
	// Existing clients use arbitrary names, including /modfs and /gomodfs.
	return h.mfs.Mount(ctx, conn, req)
}

// ToHandle implements [nfs.ReadHandler].
func (h *handler) ToHandle(f billy.Filesystem, segs []string) []byte {
	if f, ok := f.(repoFS); ok {
		header := append([]byte(handleMagic), handleVersion, byte(repoHandleKind))
		return append(header, h.gfs.ToHandle(f.Filesystem, segs)...)
	}
	return h.mfs.ToHandle(f, segs)
}

// FromHandle implements [nfs.ReadHandler].
func (h *handler) FromHandle(handle []byte) (billy.Filesystem, []string, error) {
	kind, payload, err := decodeHandle(handle)
	if err != nil {
		return nil, nil, err
	}
	if kind == repoHandleKind {
		f, segs, err := h.gfs.FromHandle(payload)
		if err != nil {
			return nil, nil, err
		}
		return repoFS{
			Filesystem: f,
		}, segs, nil
	}
	return h.mfs.FromHandle(payload)
}

// OnNFSRead implements [nfs.ReadHandler].
func (h *handler) OnNFSRead(ctx context.Context, handle []byte, offset uint64, count uint32) (*nfs.NFSReadResult, error) {
	kind, payload, err := decodeHandle(handle)
	if err != nil {
		return nil, err
	}
	if kind == repoHandleKind {
		return h.gfs.OnNFSRead(ctx, payload, offset, count)
	}
	return h.mfs.OnNFSRead(ctx, payload, offset, count)
}

func (h *handler) FSStat(ctx context.Context, f billy.Filesystem, stat *nfs.FSStat) error {
	if f, ok := f.(repoFS); ok {
		return h.gfs.FSStat(ctx, f.Filesystem, stat)
	}
	return h.mfs.FSStat(ctx, f, stat)
}

// InvalidateHandle implements [nfs.ReadHandler].
func (h *handler) InvalidateHandle(f billy.Filesystem, handle []byte) error {
	kind, payload, err := decodeHandle(handle)
	if err != nil {
		return err
	}
	rf, isRepo := f.(repoFS)
	if (kind == repoHandleKind) != isRepo {
		return errStaleHandle
	}
	if isRepo {
		return h.gfs.InvalidateHandle(rf.Filesystem, payload)
	}
	return h.mfs.InvalidateHandle(f, payload)
}

// Change implements [nfs.ReadHandler].
func (h *handler) Change(billy.Filesystem) billy.Change {
	return nil
}

// HandleLimit implements [nfs.ReadHandler].
func (h *handler) HandleLimit() int {
	return math.MaxInt
}
