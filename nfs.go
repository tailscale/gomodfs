package gomodfs

import (
	"context"
	"log"
	"net"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs"
)

func (fs *FS) NFSHandler() nfs.Handler {
	// Create a new NFS handler that uses the gomodfs file system.
	return &NFSHandler{fs: fs}
}

type NFSHandler struct {
	fs *FS

	nfs.Handler // temporary embedding during dev to watch what panics
}

func (h *NFSHandler) Mount(ctx context.Context, c net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	log.Printf("NFS mount request from %s for %+v, %q", c.RemoteAddr(), req.Header, req.Dirpath)
	return nfs.MountStatusErrPerm, nil, nil // not implemented
}
