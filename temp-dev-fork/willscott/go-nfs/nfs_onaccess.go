package nfs

import (
	"bytes"
	"context"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

func onAccess(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = opAttrErrorFormatter
	roothandle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	fs, path, err := userHandle.FromHandle(roothandle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	mask, err := xdr.ReadUint32(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	attr := tryStat(fs, path)
	if err := WritePostOpAttrs(writer, attr); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	oldMask := mask
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		mask = mask & (1 | 2 | 0x20)
	}
	mask2 := mask
	mask2 |= 1 // can read
	mask2 |= 2 // can lookup
	if attr != nil && attr.Type == FileTypeDirectory {
		mask2 |= 0x20 // can search
	}

	if err := xdr.Write(writer, mask2); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	Log.Debugf("onAccess: %s, mask: %d -> %d -> %d, attr: %+v", fs.Join(path...), oldMask, mask, mask2, attr)
	return nil
}
