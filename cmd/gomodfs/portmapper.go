package main

import (
	"log"
	"net"

	"github.com/tailscale/gomodfs/portmap"
)

const (
	rpcBindAddr = ":111"
	staticPort  = 2049
)

func startPortmapper() error {
	tcpLn, err := net.Listen("tcp", rpcBindAddr)
	if err != nil {
		return err
	}
	udpConn, err := net.ListenPacket("udp", rpcBindAddr)
	if err != nil {
		tcpLn.Close()
		return err
	}
	pm := &portmap.Server{Port: staticPort, Logf: log.Printf}
	log.Printf("portmap-static: listening on TCP and UDP %s", rpcBindAddr)
	go func() {
		log.Printf("portmap-static: TCP: %v", pm.ServeTCP(tcpLn))
	}()
	go func() {
		log.Printf("portmap-static: UDP: %v", pm.ServeUDP(udpConn))
	}()
	return nil
}
