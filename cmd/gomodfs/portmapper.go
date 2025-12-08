package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
)

func startPortmapper() error {
	tcpLn, err := net.Listen("tcp", rpcBindAddr)
	if err != nil {
		return err
	}
	go runPortMapperTCP(tcpLn)

	udpAddr := mustResolveUDP(rpcBindAddr)
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	go runPortMapperUDP(udpConn)
	return nil
}

func runPortMapperTCP(ln net.Listener) {
	defer ln.Close()

	log.Printf("portmap-static: listening on TCP %s", ln.Addr())

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("runPortMapperTCP.Accept: %v", err)
			return
		}
		handleRPCBindConn(conn)
	}
}

func runPortMapperUDP(conn *net.UDPConn) {
	defer conn.Close()
	log.Printf("portmap-static: listening on UDP %s", conn.LocalAddr())

	buf := make([]byte, 64*1024)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("runPortMapperUDP: %v", err)
			return
		}
		handleUDPPacket(conn, addr, buf[:n])
	}
}

const (
	rpcBindAddr = ":111"
	staticPort  = 2049

	rpcCall  = 0
	rpcReply = 1

	replyMsgAccepted = 0
	authNull         = 0
	acceptSuccess    = 0

	portmapProg = 100000
	portmapVers = 2

	pmapProcNull    = 0
	pmapprocGetport = 3

	protTCP = 6
	protUDP = 17

	nfsProg   = 100003
	mountProg = 100005
	nlmProg   = 100021
)

func mustResolveUDP(addr string) *net.UDPAddr {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	return ua
}

func handleRPCBindConn(c net.Conn) {
	log.Printf("rpcbind: accepted TCP connection from %s", c.RemoteAddr())
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		rec, err := nfs.ReadRPCRecord(br)
		log.Printf("rpcbind: TCP conn %s sent rec % 02x", c.RemoteAddr(), rec)
		if err != nil {
			if err != io.EOF {
				log.Printf("rpcbind: readRPCRecord: %v", err)
			}
			return
		}
		handleTCPPacket(c, rec)
	}
}

func handleTCPPacket(c net.Conn, data []byte) {
	log.Printf("rpcbind: received %d bytes from TCP %s: % 02x", len(data), c.RemoteAddr(), data)
	handlePacket(data, func(resp []byte) error {
		var buf [4]byte
		const lastFragMask = 1 << 31
		binary.BigEndian.PutUint32(buf[:], uint32(len(resp))|lastFragMask)
		_, err := fmt.Fprintf(c, "%s%s", buf[:], resp)
		return err
	})
}

func handleUDPPacket(conn *net.UDPConn, addr *net.UDPAddr, data []byte) {
	log.Printf("rpcbind: received %d bytes from UDP %s: % 02x", len(data), addr, data)
	handlePacket(data, func(resp []byte) error {
		_, err := conn.WriteToUDP(resp, addr)
		return err
	})
}

func handlePacket(data []byte, sendResp func([]byte) error) {
	r := bytes.NewReader(data)

	var xid, mtype, rpcvers, prog, vers, proc uint32
	if err := binary.Read(r, binary.BigEndian, &xid); err != nil {
		return
	}
	if err := binary.Read(r, binary.BigEndian, &mtype); err != nil || mtype != rpcCall {
		return
	}
	if err := binary.Read(r, binary.BigEndian, &rpcvers); err != nil || rpcvers != 2 {
		return
	}
	if err := binary.Read(r, binary.BigEndian, &prog); err != nil || prog != portmapProg {
		log.Printf("portmap-static: unknown program %02x; want %02x", prog, portmapProg)
		return
	}
	if err := binary.Read(r, binary.BigEndian, &vers); err != nil || vers != portmapVers {
		return
	}
	if err := binary.Read(r, binary.BigEndian, &proc); err != nil {
		return
	}

	log.Printf("portmap-static: RPC call rpcver=%v xid=%d prog=%d vers=%d proc=%d", rpcvers, xid, prog, vers, proc)

	if !skipOpaqueAuth(r) { // cred
		log.Printf("portmap-static: failed to skip cred")
		return
	}
	if !skipOpaqueAuth(r) { // verf
		log.Printf("portmap-static: failed to skip cred")
		return
	}

	if proc == pmapProcNull {
		// Reply with empty response
		var buf bytes.Buffer
		writeReplySuccess(&buf, xid)
		resp := buf.Bytes()
		log.Printf("rpcbind: replying to null ping")
		if err := sendResp(resp); err != nil {
			log.Printf("rpcbind: sendResp: %v", err)
		}
		return
	}

	if proc != pmapprocGetport {
		log.Printf("portmap-static: unknown procedure %08x; want %08x", proc, pmapprocGetport)
		return
	}

	// Portmap v2 pmap args: prog, vers, prot, port
	var argProg, argVers, argProt, argPort uint32
	if err := binary.Read(r, binary.BigEndian, &argProg); err != nil {
		return
	}
	if err := binary.Read(r, binary.BigEndian, &argVers); err != nil {
		return
	}
	if err := binary.Read(r, binary.BigEndian, &argProt); err != nil {
		return
	}
	if err := binary.Read(r, binary.BigEndian, &argPort); err != nil {
		return
	}
	_ = argVers
	_ = argProt
	_ = argPort

	log.Printf("rpcbind: GetPort prog=%d vers=%d prot=%d port=%d", argProg, argVers, argProt, argPort)

	var port uint32
	switch argProt {
	case protUDP:
		port = 0 // don't advertise UDP support
	case protTCP:
		switch argProg {
		case nfsProg, mountProg, nlmProg:
			port = staticPort
		}
	}

	resp := buildPortReply(xid, port)
	log.Printf("rpcbind: replying with port %d: % 02x", port, resp)
	if err := sendResp(resp); err != nil {
		log.Printf("rpcbind: sendResp: %v", err)
	}
}

func skipOpaqueAuth(r *bytes.Reader) bool {
	var flavor, length uint32
	if err := binary.Read(r, binary.BigEndian, &flavor); err != nil {
		return false
	}
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return false
	}
	_ = flavor
	pad := (length + 3) &^ 3
	if pad == 0 {
		return true
	}
	_, err := r.Seek(int64(pad), 1)
	return err == nil
}

func writeReplySuccess(buf *bytes.Buffer, xid uint32) {
	// rpc_msg
	_ = binary.Write(buf, binary.BigEndian, xid)                      // xid
	_ = binary.Write(buf, binary.BigEndian, uint32(rpcReply))         // mtype = REPLY
	_ = binary.Write(buf, binary.BigEndian, uint32(replyMsgAccepted)) // reply_stat = MSG_ACCEPTED

	// verf: AUTH_NULL, length 0
	_ = binary.Write(buf, binary.BigEndian, uint32(authNull))
	_ = binary.Write(buf, binary.BigEndian, uint32(0))

	_ = binary.Write(buf, binary.BigEndian, uint32(acceptSuccess))
}

func buildPortReply(xid, port uint32) []byte {
	var buf bytes.Buffer
	writeReplySuccess(&buf, xid)

	// result: uint32 port
	_ = binary.Write(&buf, binary.BigEndian, port)

	return buf.Bytes()
}
