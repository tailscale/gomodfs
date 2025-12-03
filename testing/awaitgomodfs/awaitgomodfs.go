// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The awaitgomodfs binary is used in CI tests to wait for a gomodfs server
// to come up on localhost.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

var (
	waitAddr = flag.String("wait-addr", "127.0.0.1:8080", "if set, sets the address for TestAwaitLocalhostGomodfsUp to wait on")
)

func main() {
	flag.Parse()
	t0 := time.Now()
	for range 10 {
		resp, err := http.DefaultClient.Head("http://" + *waitAddr + "/metrics")
		if resp != nil {
			resp.Body.Close()
			ct := resp.Header.Get("Content-Type")
			if resp.StatusCode == 200 {
				fmt.Printf("up; content-type: %q\n", ct)
				return
			}
			fmt.Printf("not up yet: status %v\n", resp.Status)
		} else if err != nil {
			fmt.Printf("not up yet: %v\n", err)
		}
		time.Sleep(1 * time.Second)
	}
	log.Fatalf("localhost gomodfs server not up after %v\n", time.Since(t0))
}
