package nfs

import (
	"bytes"
	"context"

	"github.com/willscott/go-nfs-client/nfs/xdr"
)

const (
	mountServiceID = 100005
)

func init() {
	_ = RegisterMessageHandler(mountServiceID, uint32(MountProcNull), onMountNull)
	_ = RegisterMessageHandler(mountServiceID, uint32(MountProcMount), onMount)
	_ = RegisterMessageHandler(mountServiceID, uint32(MountProcUmnt), onUMount)
	_ = RegisterMessageHandler(mountServiceID, uint32(MountProcUmntAll), onUMountAll)
	_ = RegisterMessageHandler(mountServiceID, uint32(MountProcExport), onMountExport)
}

func onMountNull(ctx context.Context, w *response, userHandle Handler) error {
	return w.writeHeader(ResponseCodeSuccess)
}

func onMount(ctx context.Context, w *response, userHandle Handler) error {
	// TODO: auth check.
	dirpath, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return err
	}
	mountReq := MountRequest{Header: w.req.Header, Dirpath: dirpath}
	status, handle, flavors := userHandle.Mount(ctx, w.conn, mountReq)

	if err := w.writeHeader(ResponseCodeSuccess); err != nil {
		return err
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(status)); err != nil {
		return err
	}

	rootHndl := userHandle.ToHandle(handle, []string{})

	if status == MountStatusOk {
		_ = xdr.Write(writer, rootHndl)
		_ = xdr.Write(writer, flavors)
	}
	return w.Write(writer.Bytes())
}

func onUMount(ctx context.Context, w *response, userHandle Handler) error {
	_, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return err
	}

	return w.writeHeader(ResponseCodeSuccess)
}

func onUMountAll(ctx context.Context, w *response, userHandle Handler) error {
	return w.writeHeader(ResponseCodeSuccess)
}

func onMountExport(ctx context.Context, w *response, userHandle Handler) error {
	if err := w.writeHeader(ResponseCodeSuccess); err != nil {
		return err
	}

	// Build one export entry:
	// ex_dir   = "gomodfs"
	// ex_groups = NULL
	// ex_next   = NULL
	var buf bytes.Buffer

	// ex_dir (string)
	if err := xdr.Write(&buf, "gomodfs"); err != nil {
		return err
	}

	// ex_groups = NULL (no groups present)
	if err := xdr.Write(&buf, false); err != nil {
		return err
	}
	// ex_next = NULL (no next entry)
	if err := xdr.Write(&buf, false); err != nil {
		return err
	}

	return w.Write(buf.Bytes())
}
