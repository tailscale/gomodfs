package nfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	xdr2 "github.com/rasky/go-xdr/xdr2"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

var (
	// ErrInputInvalid is returned when input cannot be parsed
	ErrInputInvalid = errors.New("invalid input")
	// ErrAlreadySent is returned when writing a header/status multiple times
	ErrAlreadySent = errors.New("response already started")
)

// ResponseCode is a combination of accept_stat and reject_stat.
type ResponseCode uint32

// ResponseCode Codes
const (
	ResponseCodeSuccess ResponseCode = iota
	ResponseCodeProgUnavailable
	ResponseCodeProcUnavailable
	ResponseCodeGarbageArgs
	ResponseCodeSystemErr
	ResponseCodeRPCMismatch
	ResponseCodeAuthError
)

type conn struct {
	*Server
	writeSerializer chan []byte
	net.Conn
}

func (c *conn) serve(ctx context.Context) {
	defer c.Close()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	const concurrentHandlers = 32
	c.writeSerializer = make(chan []byte, concurrentHandlers)
	go c.serializeWrites(connCtx)

	concurrentHandlerSem := make(chan bool, concurrentHandlers)

	bio := bufio.NewReader(c.Conn)
	for {
		w, err := c.readRequestHeader(connCtx, bio)
		if err != nil {
			if err == io.EOF {
				// Clean close.
				c.Close()
				return
			}
			return
		}
		Log.Tracef("request: %v", w.req)
		select {
		case concurrentHandlerSem <- true:
		case <-connCtx.Done():
			return
		}

		go func() {
			defer func() { <-concurrentHandlerSem }()
			err := c.handle(connCtx, w)
			if err != nil {
				Log.Errorf("error handling req: %v", err)
				cancel() // failure to handle at a level needing to close the connection.
				return
			}
			respErr := w.finish(connCtx)
			if respErr != nil {
				Log.Errorf("error sending response: %v", respErr)
				cancel() // failure to handle at a level needing to close the connection.
				return
			}
		}()
	}
}

func (c *conn) serializeWrites(ctx context.Context) {
	// todo: maybe don't need the extra buffer
	bw := bufio.NewWriter(c.Conn)
	var fragmentBuf [4]byte
	var fragmentInt uint32
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.writeSerializer:
			if !ok {
				return
			}
			// prepend the fragmentation header
			fragmentInt = uint32(len(msg))
			fragmentInt |= (1 << 31)
			binary.BigEndian.PutUint32(fragmentBuf[:], fragmentInt)
			n, err := bw.Write(fragmentBuf[:])
			if n < 4 || err != nil {
				return
			}
			n, err = bw.Write(msg)
			if err != nil {
				return
			}
			if n < len(msg) {
				panic("todo: ensure writes complete fully.")
			}
			if err = bw.Flush(); err != nil {
				return
			}
		}
	}
}

// Handle a request. errors from this method indicate a failure to read or
// write on the network stream, and trigger a disconnection of the connection.
func (c *conn) handle(ctx context.Context, w *response) error {
	handler := c.Server.handlerFor(w.req.Header.Prog, w.req.Header.Proc)
	if handler == nil {
		Log.Errorf("No handler for %d.%d", w.req.Header.Prog, w.req.Header.Proc)
		return c.err(ctx, w, &ResponseCodeProcUnavailableError{})
	}
	appError := handler(ctx, w, c.Server.Handler)
	if appError != nil && !w.responded {
		if err := c.err(ctx, w, appError); err != nil {
			return err
		}
	}
	if !w.responded {
		Log.Errorf("Handler did not indicate response status via writing or erroring")
		if err := c.err(ctx, w, &ResponseCodeSystemError{}); err != nil {
			return err
		}
	}
	return nil
}

func (c *conn) err(ctx context.Context, w *response, err error) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	if w.err == nil {
		w.err = err
	}

	if w.responded {
		return nil
	}

	rpcErr := w.errorFmt(err)
	if writeErr := w.writeHeader(rpcErr.Code()); writeErr != nil {
		return writeErr
	}

	body, _ := rpcErr.MarshalBinary()
	return w.Write(body)
}

type request struct {
	xid uint32
	rpc.Header
	Body io.Reader
}

func (r *request) String() string {
	switch r.Header.Prog {
	case nfsServiceID:
		return fmt.Sprintf("RPC #%d (nfs.%s)", r.xid, NFSProcedure(r.Header.Proc))
	case mountServiceID:
		return fmt.Sprintf("RPC #%d (mount.%s)", r.xid, MountProcedure(r.Header.Proc))
	case lockServiceID:
		return fmt.Sprintf("RPC #%d (lock.%d)", r.xid, r.Header.Proc)
	}
	return fmt.Sprintf("RPC #%d (%d.%d)", r.xid, r.Header.Prog, r.Header.Proc)
}

type response struct {
	*conn
	writer    *bytes.Buffer
	responded bool
	err       error
	errorFmt  func(error) RPCError
	req       *request
}

func (w *response) writeXdrHeader() error {
	err := xdr.Write(w.writer, &w.req.xid)
	if err != nil {
		return err
	}
	respType := uint32(1)
	err = xdr.Write(w.writer, &respType)
	if err != nil {
		return err
	}
	return nil
}

func (w *response) writeHeader(code ResponseCode) error {
	if w.responded {
		return ErrAlreadySent
	}
	w.responded = true
	if err := w.writeXdrHeader(); err != nil {
		return err
	}

	status := rpc.MsgAccepted
	if code == ResponseCodeAuthError || code == ResponseCodeRPCMismatch {
		status = rpc.MsgDenied
	}

	err := xdr.Write(w.writer, &status)
	if err != nil {
		return err
	}

	if status == rpc.MsgAccepted {
		// Write opaque_auth header.
		err = xdr.Write(w.writer, &rpc.AuthNull)
		if err != nil {
			return err
		}
	}

	return xdr.Write(w.writer, &code)
}

// Write a response to an xdr message
func (w *response) Write(dat []byte) error {
	if !w.responded {
		if err := w.writeHeader(ResponseCodeSuccess); err != nil {
			return err
		}
	}

	acc := 0
	for acc < len(dat) {
		n, err := w.writer.Write(dat[acc:])
		if err != nil {
			return err
		}
		acc += n
	}
	return nil
}

func (w *response) finish(ctx context.Context) error {
	select {
	case w.conn.writeSerializer <- w.writer.Bytes():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

const lastFragMask = 1 << 31
const maxRPCSize = 10 << 20 // 10MB

func ReadRPCRecord(br *bufio.Reader) ([]byte, error) {
	var msg []byte
	for {
		frag, err := xdr.ReadUint32(br)
		if err != nil {
			if xdrErr, ok := err.(*xdr2.UnmarshalError); ok {
				if xdrErr.Err == io.EOF {
					return nil, io.EOF
				}
			}
			return nil, err
		}

		last := frag&lastFragMask != 0
		n := int64(frag &^ lastFragMask)
		if n+int64(len(msg)) > maxRPCSize {
			return nil, fmt.Errorf("fragment size %d too large", n)
		}

		// TODO(bradfitz): optimize for common case of single-fragment messages
		// by avoiding the extra allocation and copy and just use br.Peek(n),
		// get the memory, then br.Discard(n), knowing the buffer is still valid
		// until used again. And then just document that limitation.

		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		if msg == nil && last {
			// Common case: single-fragment message
			return buf, nil
		}
		msg = append(msg, buf...)
		if last {
			return msg, nil
		}
	}
}

func (c *conn) readRequestHeader(ctx context.Context, reader *bufio.Reader) (w *response, err error) {
	rpcMsg, err := ReadRPCRecord(reader)
	if err != nil {
		return nil, err
	}
	if len(rpcMsg) < 40 {
		return nil, ErrInputInvalid
	}

	r := bytes.NewReader(rpcMsg)

	xid, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	reqType, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if reqType != 0 { // 0 = request, 1 = response
		return nil, ErrInputInvalid
	}

	req := request{
		xid,
		rpc.Header{},
		r,
	}
	if err = xdr.Read(r, &req.Header); err != nil {
		return nil, err
	}

	w = &response{
		conn:     c,
		req:      &req,
		errorFmt: basicErrorFormatter,
		// TODO: use a pool for these.
		writer: bytes.NewBuffer([]byte{}),
	}
	return w, nil
}
