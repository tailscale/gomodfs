// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package gomodfs provides tests for sending raw TCP bytestreams to test NFS RPC calls.
//
// This test allows you to specify raw byte streams (as hex strings) containing
// one or more RPC messages. The test will send the raw bytes to the NFS server
// and parse the responses.

package gomodfs

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	nfs "github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// TestRawByteStream tests the NFS server with user-provided raw byte streams
func TestRawByteStream(t *testing.T) {
	// Set up NFS server
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	// Create a gomodfs instance
	fs := &FS{}

	handler := fs.NFSHandler()

	go func() {
		_ = nfs.Serve(listener, handler)
	}()

	time.Sleep(100 * time.Millisecond)

	// Get a valid file handle from the server for the status file
	c, err := rpc.DialTCP(listener.Addr().Network(), listener.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var mounter nfsc.Mount
	mounter.Client = c
	target, err := mounter.Mount("/", rpc.AuthNull)
	if err != nil {
		t.Fatal(err)
	}
	defer mounter.Unmount()

	_, statusFileHandle, err := target.Lookup("/status.json")
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Got valid status file handle: %d bytes", len(statusFileHandle))
	t.Logf("Handle hex: %x", statusFileHandle)

	// Build example raw byte streams
	// Note: These are complete byte streams you can copy and modify

	tests := []struct {
		name              string
		rawBytes          string // Raw hex string of the byte stream
		expectedResponses int
		expectedStatuses  []uint32 // Expected NFS status for each response (0=OK, 70=STALE)
		description       string
	}{
		{
			name: "repro",
			rawBytes: `
				80000098
				1ddbda15
				00000000
				00000002
				000186a3
				00000003
				00000001
				00000001
				0000002c
				00000000
				00000009
				66632d75
				62756e74
				75000000
				000003e8
				000003e8
				00000003
				0000001b
				000003e5
				000003e8
				00000000
				00000000
                00000040
				00000000 00000000 00000000 00000000  // First 32 bytes (module hash, zeros for status file)
				00000000 00000000 00000000 00000000
				87a747fc a80a8035 a39efd45 651b0285  // Second 32 bytes (path hash for status.json)
				da341df0 0389abfd 66705f2d 3af8839d

				// 2nd RPC message
                800000a81edbda150000000000000002000186a30000000300000003000000010000002c000000000000000966632d7562756e7475000000000003e8000003e8000000030000001b000003e5000003e80000000000000000


				// LOOKUP3args: directory handle (root) + filename
				00000040        // Handle length: 64 bytes
				00000000 00000000 00000000 00000000  // Root directory = all zeros
				00000000 00000000 00000000 00000000
				00000000 00000000 00000000 00000000
				00000000 00000000 00000000 00000000
				0000000b        // Filename length: 11
				7374617475732e6a736f6e00  // "status.json"

				// 3rd RPC message
				800000a41fdbda150000000000000002000186a30000000300000006000000010000002c000000000000000966632d7562756e7475000000000003e8000003e8000000030000001b000003e5000003e80000000000000000

				// 3rd NFS message
				00000040
				00000000 00000000 00000000 00000000  // First 32 bytes (module hash, zeros for status file)
				00000000 00000000 00000000 00000000
				87a747fc a80a8035 a39efd45 651b0285  // Second 32 bytes (path hash for status.json)
				da341df0 0389abfd 66705f2d 3af8839d
				0000000000000000
				00001000

				// 4th RPC message
				8000009c20dbda150000000000000002000186a30000000300000004000000010000002c000000000000000966632d7562756e7475000000000003e8000003e8000000030000001b000003e5000003e80000000000000000

				// 4th NFS message
				00000040
				00000000 00000000 00000000 00000000  // First 32 bytes (module hash, zeros for status file)
				00000000 00000000 00000000 00000000
				87a747fc a80a8035 a39efd45 651b0285  // Second 32 bytes (path hash for status.json)
				da341df0 0389abfd 66705f2d 3af8839d
				0000001f

				// 5th RPC message
				8000009821dbda150000000000000002000186a30000000300000001000000010000002c000000000000000966632d7562756e7475000000000003e8000003e8000000030000001b000003e5000003e80000000000000000

				// 5th NFS message
                00000040
				00000000 00000000 00000000 00000000  // First 32 bytes (module hash, zeros for status file)
				00000000 00000000 00000000 00000000
				87a747fc a80a8035 a39efd45 651b0285  // Second 32 bytes (path hash for status.json)
				da341df0 0389abfd 66705f2d 3af8839d
			`,
			expectedResponses: 5,
			expectedStatuses:  []uint32{0, 0, 10006, 0, 0}, // GETATTR=OK, LOOKUP=OK, READ=ServerFault
			description:       "GETATTR on real /status.json - should return NFS_OK (status=0)",
		},
		{
			name: "GETATTR with real 64-byte handle for status.json",
			rawBytes: `
				// GETATTR call (XID=5000) on /status.json
				8000006c        // Fragment: last=1, len=108 (40 + 4 + 64)
				00001388        // XID: 5000
				00000000        // CALL
				00000002        // RPC version 2
				000186a3        // Program: NFS (100003)
				00000003        // Version: 3
				00000001        // Procedure: GETATTR
				00000000        // Auth: NULL
				00000000
				00000000        // Verf: NULL
				00000000
				00000040        // File handle length: 64 bytes (0x40)
				00000000 00000000 00000000 00000000  // First 32 bytes (module hash, zeros for status file)
				00000000 00000000 00000000 00000000
				87a747fc a80a8035 a39efd45 651b0285  // Second 32 bytes (path hash for status.json)
				da341df0 0389abfd 66705f2d 3af8839d
			`,
			expectedResponses: 1,
			expectedStatuses:  []uint32{0}, // NFS_OK
			description:       "GETATTR on real /status.json - should return NFS_OK (status=0)",
		},
		{
			name: "Two messages: NULL then GETATTR",
			rawBytes: `
				// Message 1: NULL call (XID=1000)
				80000028        // Fragment: last=1, len=40
				000003e8        // XID: 1000
				00000000        // CALL
				00000002        // RPC version 2
				000186a3        // Program: NFS (100003)
				00000003        // Version: 3
				00000000        // Procedure: NULL
				00000000        // Auth: NULL
				00000000
				00000000        // Verf: NULL
				00000000

				// Message 2: GETATTR call (XID=1001)
				8000004c        // Fragment: last=1, len=76 (40 byte header + 4 byte handle length + 32 byte handle)
				000003e9        // XID: 1001
				00000000        // CALL
				00000002        // RPC version 2
				000186a3        // Program: NFS (100003)
				00000003        // Version: 3
				00000001        // Procedure: GETATTR
				00000000        // Auth: NULL
				00000000
				00000000        // Verf: NULL
				00000000
				00000020        // File handle length: 32 bytes
				11111111 22222222 33333333 44444444
				55555555 66666666 77777777 88888888
			`,
			expectedResponses: 2,
			expectedStatuses:  []uint32{0, 70}, // NULL=OK, GETATTR with fake handle=STALE
			description:       "Stream containing NULL call followed by GETATTR on root directory",
		},
		{
			name: "Three messages: GETATTR, NULL, GETATTR",
			rawBytes: `
				// Message 1: GETATTR (XID=2000)
				8000004c
				000007d0
				00000000
				00000002
				000186a3
				00000003
				00000001
				00000000
				00000000
				00000000
				00000000
				00000020
				11111111 22222222 33333333 44444444
				55555555 66666666 77777777 88888888

				// Message 2: NULL (XID=2001)
				80000028
				000007d1
				00000000
				00000002
				000186a3
				00000003
				00000000
				00000000
				00000000
				00000000
				00000000

				// Message 3: GETATTR (XID=2002)
				8000004c
				000007d2
				00000000
				00000002
				000186a3
				00000003
				00000001
				00000000
				00000000
				00000000
				00000000
				00000020
				11111111 22222222 33333333 44444444
				55555555 66666666 77777777 88888888
			`,
			expectedResponses: 3,
			expectedStatuses:  []uint32{70, 0, 70}, // GETATTR=STALE, NULL=OK, GETATTR=STALE
			description:       "Stream with GETATTR, NULL, GETATTR",
		},
		{
			name: "Five messages with multiple GETATTRs",
			rawBytes: `
				// Message 1: NULL (XID=3000)
				80000028 00000bb8 00000000 00000002 000186a3 00000003 00000000 00000000 00000000 00000000 00000000
				// Message 2: GETATTR (XID=3001)
				8000004c 00000bb9 00000000 00000002 000186a3 00000003 00000001 00000000 00000000 00000000 00000000 00000020 11111111 22222222 33333333 44444444 55555555 66666666 77777777 88888888
				// Message 3: GETATTR (XID=3002)
				8000004c 00000bba 00000000 00000002 000186a3 00000003 00000001 00000000 00000000 00000000 00000000 00000020 11111111 22222222 33333333 44444444 55555555 66666666 77777777 88888888
				// Message 4: NULL (XID=3003)
				80000028 00000bbb 00000000 00000002 000186a3 00000003 00000000 00000000 00000000 00000000 00000000
				// Message 5: GETATTR (XID=3004)
				8000004c 00000bbc 00000000 00000002 000186a3 00000003 00000001 00000000 00000000 00000000 00000000 00000020 11111111 22222222 33333333 44444444 55555555 66666666 77777777 88888888
			`,
			expectedResponses: 5,
			expectedStatuses:  []uint32{0, 70, 70, 0, 70}, // NULL=OK, GETATTR=STALE, GETATTR=STALE, NULL=OK, GETATTR=STALE
			description:       "Stream with 5 messages: NULL, GETATTR, GETATTR, NULL, GETATTR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawBytes := mustDecodeHex(tt.rawBytes)

			t.Logf("Description: %s", tt.description)
			t.Logf("Raw byte stream (%d bytes, expecting %d responses):", len(rawBytes), tt.expectedResponses)
			t.Logf("Hex dump:\n%s", hex.Dump(rawBytes))

			conn, err := net.DialTimeout("tcp", listener.Addr().String(), 5*time.Second)
			if err != nil {
				t.Fatalf("Failed to dial: %v", err)
			}
			defer conn.Close()

			conn.SetDeadline(time.Now().Add(10 * time.Second))

			// Send the raw byte stream
			n, err := conn.Write(rawBytes)
			if err != nil {
				t.Fatalf("Failed to write stream: %v", err)
			}
			t.Logf("Sent %d bytes", n)

			// Read and parse responses
			for i := 0; i < tt.expectedResponses; i++ {
				response, xid, nfsStatus, err := readResponse(conn)
				if err != nil {
					t.Fatalf("Failed to read response %d: %v", i+1, err)
				}

				t.Logf("Response %d: XID=%d, NFS_status=%d, size=%d bytes",
					i+1, xid, nfsStatus, len(response))

				// Check expected NFS status if provided
				if len(tt.expectedStatuses) > 0 {
					if i >= len(tt.expectedStatuses) {
						t.Fatalf("Response %d: no expected status defined (have %d expected statuses)", i+1, len(tt.expectedStatuses))
					}
					expectedStatus := tt.expectedStatuses[i]
					if nfsStatus != expectedStatus {
						t.Errorf("Response %d: got NFS_status=%d, want %d", i+1, nfsStatus, expectedStatus)
					} else {
						statusName := "NFS_OK"
						switch expectedStatus {
						case 70:
							statusName = "NFSERR_STALE"
						case 10006:
							statusName = "NFSERR_SERVERFAULT"
						}
						t.Logf("✓ Response %d: NFS status correct (%s)", i+1, statusName)
					}
				}
			}

			t.Logf("✓ Successfully processed all %d responses from raw byte stream", tt.expectedResponses)
		})
	}
}

// mustDecodeHex decodes a hex string, ignoring whitespace and comments
func mustDecodeHex(s string) []byte {
	cleaned := ""
	for _, line := range bytes.Split([]byte(s), []byte("\n")) {
		// Remove comments (everything after //)
		if idx := bytes.Index(line, []byte("//")); idx >= 0 {
			line = line[:idx]
		}
		// Remove all whitespace
		line = bytes.ReplaceAll(line, []byte(" "), []byte{})
		line = bytes.ReplaceAll(line, []byte("\t"), []byte{})
		cleaned += string(line)
	}

	if cleaned == "" {
		return []byte{}
	}

	result, err := hex.DecodeString(cleaned)
	if err != nil {
		panic(fmt.Sprintf("failed to decode hex: %v", err))
	}
	return result
}

// readResponse reads and parses a single RPC response from the connection
func readResponse(conn net.Conn) ([]byte, uint32, uint32, error) {
	// Read fragment header
	fragHeader := make([]byte, 4)
	_, err := io.ReadFull(conn, fragHeader)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read fragment header: %w", err)
	}

	fragment := binary.BigEndian.Uint32(fragHeader)
	respLen := fragment &^ (1 << 31)
	lastFragment := fragment&(1<<31) != 0

	if !lastFragment {
		return nil, 0, 0, fmt.Errorf("multi-fragment responses not supported")
	}

	// Read the response body
	respBody := make([]byte, respLen)
	_, err = io.ReadFull(conn, respBody)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read response body: %w", err)
	}

	reader := bytes.NewReader(respBody)

	// Parse XID
	xid, err := xdr.ReadUint32(reader)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read XID: %w", err)
	}

	// Parse message type
	msgType, err := xdr.ReadUint32(reader)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read message type: %w", err)
	}

	if msgType != 1 {
		return nil, 0, 0, fmt.Errorf("expected REPLY (1), got message type %d", msgType)
	}

	// Parse reply status
	replyStatus, err := xdr.ReadUint32(reader)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read reply status: %w", err)
	}

	if replyStatus != 0 {
		return nil, 0, 0, fmt.Errorf("reply status: %d (not MSG_ACCEPTED)", replyStatus)
	}

	// Skip auth
	var auth rpc.Auth
	if err := xdr.Read(reader, &auth); err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read auth: %w", err)
	}

	// Read accept status
	acceptStatus, err := xdr.ReadUint32(reader)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read accept status: %w", err)
	}

	if acceptStatus != 0 {
		fullResponse := append(fragHeader, respBody...)
		return fullResponse, xid, 0, nil
	}

	// Read NFS status (for NFS procedures that have it)
	var nfsStatus uint32
	if err := xdr.Read(reader, &nfsStatus); err != nil {
		nfsStatus = 0
	}

	fullResponse := append(fragHeader, respBody...)
	return fullResponse, xid, nfsStatus, nil
}
