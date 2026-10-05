// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package portmap

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// call returns a portmapper call for procedure proc, with GETPORT arguments
// for prog and prot. It has an AUTH_UNIX credential, as Windows sends, to check
// that the credential is skipped correctly.
func call(xid, proc, prog, prot uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, xid)
	for _, v := range []uint32{rpcCall, rpcVersion, portmapProg, portmapVers, proc} {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	cred := []byte("unix credential!")      // 16 bytes, already padded
	b = binary.BigEndian.AppendUint32(b, 1) // AUTH_UNIX
	b = binary.BigEndian.AppendUint32(b, uint32(len(cred)))
	b = append(b, cred...)
	b = binary.BigEndian.AppendUint32(b, 0) // verifier: AUTH_NONE
	b = binary.BigEndian.AppendUint32(b, 0)
	if proc == pmapProcGetport {
		for _, v := range []uint32{prog, 3, prot, 0} {
			b = binary.BigEndian.AppendUint32(b, v)
		}
	}
	return b
}

func TestReply(t *testing.T) {
	s := &Server{Port: 2050, Logf: t.Logf}
	for _, tc := range []struct {
		name     string
		msg      []byte
		wantNil  bool
		wantPort int // -1 for a reply with no port (NULL)
	}{
		{"nfs-tcp", call(7, pmapProcGetport, nfsProg, ipprotoTCP), false, 2050},
		{"mount-tcp", call(7, pmapProcGetport, mountProg, ipprotoTCP), false, 2050},
		{"nlm-tcp", call(7, pmapProcGetport, nlmProg, ipprotoTCP), false, 2050},
		{"nfs-udp", call(7, pmapProcGetport, nfsProg, 17), false, 0},
		{"other-prog", call(7, pmapProcGetport, 100024, ipprotoTCP), false, 0},
		{"null", call(7, pmapProcNull, 0, 0), false, -1},
		{"other-proc", call(7, 4, 0, 0), true, 0},
		{"truncated", call(7, pmapProcGetport, nfsProg, ipprotoTCP)[:50], true, 0},
		{"empty", nil, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.reply(tc.msg)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got reply % x; want none", got)
				}
				return
			}
			wantLen := 24
			if tc.wantPort >= 0 {
				wantLen += 4
			}
			if len(got) != wantLen {
				t.Fatalf("reply is %d bytes; want %d", len(got), wantLen)
			}
			if xid := binary.BigEndian.Uint32(got); xid != 7 {
				t.Errorf("xid = %d; want 7", xid)
			}
			if mtype := binary.BigEndian.Uint32(got[4:]); mtype != rpcReply {
				t.Errorf("message type = %d; want %d", mtype, rpcReply)
			}
			if tc.wantPort >= 0 {
				if port := binary.BigEndian.Uint32(got[24:]); port != uint32(tc.wantPort) {
					t.Errorf("port = %d; want %d", port, tc.wantPort)
				}
			}
		})
	}
}

func TestServeConn(t *testing.T) {
	s := &Server{Port: 2050}
	c1, c2 := net.Pipe()
	defer c1.Close()
	go s.serveConn(c2)

	// Send the call split across two record fragments.
	msg := call(9, pmapProcGetport, nfsProg, ipprotoTCP)
	go func() {
		c1.Write(binary.BigEndian.AppendUint32(nil, 10))
		c1.Write(msg[:10])
		c1.Write(binary.BigEndian.AppendUint32(nil, uint32(len(msg)-10)|1<<31))
		c1.Write(msg[10:])
	}()

	var hdr [4]byte
	if _, err := io.ReadFull(c1, hdr[:]); err != nil {
		t.Fatal(err)
	}
	h := binary.BigEndian.Uint32(hdr[:])
	if h&(1<<31) == 0 {
		t.Errorf("reply record header %#x lacks the last-fragment bit", h)
	}
	resp := make([]byte, h&^(1<<31))
	if _, err := io.ReadFull(c1, resp); err != nil {
		t.Fatal(err)
	}
	if len(resp) != 28 || binary.BigEndian.Uint32(resp[24:]) != 2050 {
		t.Errorf("reply % x; want 28 bytes ending in port 2050", resp)
	}
}

func TestServeUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go (&Server{Port: 2050}).ServeUDP(pc)

	c, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(call(3, pmapProcGetport, mountProg, ipprotoTCP)); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 100)
	n, err := c.Read(resp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 28 || binary.BigEndian.Uint32(resp[24:]) != 2050 {
		t.Errorf("reply % x; want 28 bytes ending in port 2050", resp[:n])
	}
}
