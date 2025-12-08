package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/willscott/go-nfs-client/nfs/xdr"
)

const (
	lockServiceID = 100021

	LockProcNull = 0 // NLM4_NULL

	LockProcLock   = 2
	LockProcCancel = 3
	LockProcUnlock = 4

	LockProcShare      = 20 // NLM4_SHARE
	LockProcUnshare    = 21 // NLM4_UNSHARE
	LockProcNonMonLock = 22 // NLM4_NM_LOCK (non-monitored)
	LockProcFreeAll    = 23 // NLM4_FREE_ALL

	nlm4Granted = 0 // nlm4_stats: NLM4_GRANTED
)

func init() {
	_ = RegisterMessageHandler(lockServiceID, uint32(LockProcNull), onLockNull)
	_ = RegisterMessageHandler(lockServiceID, uint32(LockProcFreeAll), onLockFreeAll)

	_ = RegisterMessageHandler(lockServiceID, uint32(LockProcShare), onLockShare)
	_ = RegisterMessageHandler(lockServiceID, uint32(LockProcUnshare), onLockUnshare)

	// Proc 2, 3, 4 and 22 have the same args and response format.
	_ = RegisterMessageHandler(lockServiceID, uint32(LockProcLock), onLockUnlockCancel)
	_ = RegisterMessageHandler(lockServiceID, uint32(LockProcNonMonLock), onLockUnlockCancel)
	_ = RegisterMessageHandler(lockServiceID, uint32(LockProcCancel), onLockUnlockCancel)
	_ = RegisterMessageHandler(lockServiceID, uint32(LockProcUnlock), onLockUnlockCancel)
}

func onLockNull(ctx context.Context, w *response, userHandle Handler) error {
	return w.writeHeader(ResponseCodeSuccess)
}

func onLockFreeAll(ctx context.Context, w *response, userHandle Handler) error {
	var name string
	if err := xdr.Read(w.req.Body, &name); err != nil {
		return err
	}
	var state int32
	if err := xdr.Read(w.req.Body, &state); err != nil {
		return err
	}

	return w.writeHeader(ResponseCodeSuccess)
}

func onLockShare(ctx context.Context, w *response, userHandle Handler) error {
	return onLockShareUnshare(ctx, w, "share")
}

func onLockUnshare(ctx context.Context, w *response, userHandle Handler) error {
	return onLockShareUnshare(ctx, w, "unshare")
}

func onLockShareUnshare(ctx context.Context, w *response, action string) error {

	cookieLen, err := xdr.ReadUint32(w.req.Body)
	if err != nil {
		return err
	}
	if cookieLen > 128 {
		return errors.New("nlm: cookie too long")
	}
	if cookieLen%4 != 0 {
		return errors.New("nlm: cookie length not multiple of 4")
	}
	cookie := make([]byte, cookieLen)
	if _, err := io.ReadFull(w.req.Body, cookie); err != nil {
		return err
	}

	if err := w.writeHeader(ResponseCodeSuccess); err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, cookieLen); err != nil {
		return err
	}

	// nlm4_cookie: opaque[16], fixed-size, no length field.
	if _, err := buf.Write(cookie); err != nil {
		return err
	}
	// nlm4_stats stat (NLM4_GRANTED = 0)
	if err := binary.Write(&buf, binary.BigEndian, uint32(nlm4Granted)); err != nil {
		return err
	}
	// sequence (int32 or uint32; we just use 0)
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return err
	}

	return w.Write(buf.Bytes())
}

// Proc 2: LOCK
// Proc 3: CANCEL
// Proc 4: UNLOCK
// Proc 22: NM_LOCK
func onLockUnlockCancel(ctx context.Context, w *response, userHandle Handler) error {
	// read the cookie (netobj)
	cookieLen, err := xdr.ReadUint32(w.req.Body)
	if err != nil {
		return err
	}
	if cookieLen > 128 { // Sanity check
		return errors.New("nlm: cookie too long")
	}
	cookie := make([]byte, cookieLen)
	if _, err := io.ReadFull(w.req.Body, cookie); err != nil {
		return err
	}

	if err := w.writeHeader(ResponseCodeSuccess); err != nil {
		return err
	}

	var buf bytes.Buffer

	// Write the Reply Payload (nlm4_res):
	//   netobj cookie;
	//   nlm4_stats stat;

	if err := binary.Write(&buf, binary.BigEndian, cookieLen); err != nil {
		return err
	}
	if _, err := buf.Write(cookie); err != nil {
		return err
	}

	// The stat field, (0 = NLM4_GRANTED).
	// Unlike SHARE, there is no sequence number after this.
	if err := binary.Write(&buf, binary.BigEndian, uint32(nlm4Granted)); err != nil {
		return err
	}

	return w.Write(buf.Bytes())
}
