package nfs

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"

	rasxdr "github.com/rasky/go-xdr/xdr2"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

func Read(r io.Reader, val interface{}) error {
	_, err := rasxdr.Unmarshal(r, val)
	return err
}

func ReadUint32(r io.Reader) (uint32, error) {
	var n uint32
	if err := Read(r, &n); err != nil {
		return n, err
	}
	log.Printf("LENGTH: %d", n)

	return n, nil
}

func ReadOpaque(r io.Reader) ([]byte, error) {
	length, err := ReadUint32(r)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, length)
	if _, err = r.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func onGetAttr(ctx context.Context, w *response, userHandle Handler) error {
	handle, err := ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	log.Printf("HANDLE: %x", handle)

	fs, path, err := userHandle.FromHandle(handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}

	fullPath := fs.Join(path...)
	info, err := fs.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &NFSStatusError{NFSStatusNoEnt, err}
		}
		return &NFSStatusError{NFSStatusIO, err}
	}
	attr := ToFileAttribute(info, fullPath)
	Log.Debugf("onGetAttr: %s, attr: %+v", fullPath, attr)

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, attr); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
