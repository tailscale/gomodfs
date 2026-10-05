// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package portmap implements a static ONC RPC portmapper (rpcbind version 2,
// RFC 1833) that tells NFS clients which port gomodfs's NFS server is on.
//
// Linux and macOS NFS clients can be told the NFS and MOUNT ports directly,
// but the Windows NFS client always asks the portmapper on port 111 first, so
// serving gomodfs to Windows clients requires one.
package portmap

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

const (
	rpcCall          = 0
	rpcReply         = 1
	rpcVersion       = 2
	replyMsgAccepted = 0
	acceptSuccess    = 0

	portmapProg     = 100000
	portmapVers     = 2
	pmapProcNull    = 0
	pmapProcGetport = 3
	ipprotoTCP      = 6

	nfsProg   = 100003
	mountProg = 100005
	nlmProg   = 100021
)

// maxRecord bounds the size of an RPC call that a Server reads.
const maxRecord = 64 << 10

// Server is a static portmapper. It answers GETPORT queries for the NFS,
// MOUNT, and NLM programs over TCP with Port, and all other GETPORT queries,
// including any over UDP, with port 0, meaning "not registered".
type Server struct {
	// Port is the TCP port of the NFS server.
	Port int

	// Logf, if non-nil, logs each query and reply.
	Logf func(format string, args ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// ServeTCP serves the portmapper on ln until ln.Accept fails, and returns
// that error.
func (s *Server) ServeTCP(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serveConn(c)
	}
}

// ServeUDP serves the portmapper on pc until reading from it fails, and
// returns that error.
func (s *Server) ServeUDP(pc net.PacketConn) error {
	buf := make([]byte, maxRecord)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		if resp := s.reply(buf[:n]); resp != nil {
			if _, err := pc.WriteTo(resp, addr); err != nil {
				s.logf("portmap: replying to %v: %v", addr, err)
			}
		}
	}
}

// serveConn answers RPC calls on c, which uses RPC record marking (RFC 5531,
// section 11): each record is a series of fragments that each start with a
// 4-byte header holding the fragment length and a last-fragment bit.
func (s *Server) serveConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		rec, err := readRecord(br)
		if err != nil {
			if err != io.EOF {
				s.logf("portmap: reading from %v: %v", c.RemoteAddr(), err)
			}
			return
		}
		resp := s.reply(rec)
		if resp == nil {
			return
		}
		out := binary.BigEndian.AppendUint32(nil, uint32(len(resp))|1<<31)
		if _, err := c.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

func readRecord(br *bufio.Reader) ([]byte, error) {
	var rec []byte
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return nil, err
		}
		h := binary.BigEndian.Uint32(hdr[:])
		n := int(h &^ (1 << 31))
		if len(rec)+n > maxRecord {
			return nil, fmt.Errorf("record larger than %d bytes", maxRecord)
		}
		start := len(rec)
		rec = append(rec, make([]byte, n)...)
		if _, err := io.ReadFull(br, rec[start:]); err != nil {
			return nil, err
		}
		if h&(1<<31) != 0 {
			return rec, nil
		}
	}
}

// xdrReader reads big-endian uint32s from b, recording any short read in bad.
type xdrReader struct {
	b   []byte
	bad bool
}

func (r *xdrReader) u32() uint32 {
	if len(r.b) < 4 {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

// skipOpaqueAuth skips an RPC opaque_auth: a flavor and padded opaque body.
func (r *xdrReader) skipOpaqueAuth() {
	r.u32() // flavor
	n := (uint64(r.u32()) + 3) &^ 3
	if uint64(len(r.b)) < n {
		r.bad = true
		return
	}
	r.b = r.b[n:]
}

// reply returns the reply to the RPC call in msg, or nil if msg isn't a
// portmapper call that s answers.
func (s *Server) reply(msg []byte) []byte {
	r := &xdrReader{b: msg}
	xid := r.u32()
	if r.u32() != rpcCall || r.u32() != rpcVersion || r.u32() != portmapProg || r.u32() != portmapVers {
		return nil
	}
	proc := r.u32()
	r.skipOpaqueAuth() // credentials
	r.skipOpaqueAuth() // verifier
	var prog, vers, prot, port uint32
	switch proc {
	case pmapProcNull:
	case pmapProcGetport:
		prog, vers, prot = r.u32(), r.u32(), r.u32()
		if prot == ipprotoTCP && (prog == nfsProg || prog == mountProg || prog == nlmProg) {
			port = uint32(s.Port)
		}
	default:
		s.logf("portmap: ignoring call to procedure %d", proc)
		return nil
	}
	if r.bad {
		return nil
	}

	if proc == pmapProcGetport {
		s.logf("portmap: GETPORT prog=%d vers=%d prot=%d: port %d", prog, vers, prot, port)
	}
	b := binary.BigEndian.AppendUint32(nil, xid)
	b = binary.BigEndian.AppendUint32(b, rpcReply)
	b = binary.BigEndian.AppendUint32(b, replyMsgAccepted)
	b = binary.BigEndian.AppendUint32(b, 0) // verifier flavor: AUTH_NONE
	b = binary.BigEndian.AppendUint32(b, 0) // verifier length
	b = binary.BigEndian.AppendUint32(b, acceptSuccess)
	if proc == pmapProcGetport {
		b = binary.BigEndian.AppendUint32(b, port)
	}
	return b
}
