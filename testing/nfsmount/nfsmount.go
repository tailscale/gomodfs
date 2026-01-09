// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The startgomodfs binary is used in CI tests to start a gomodfs server on
// Windows, because Powershell-in-YAML-in-Github-Actions with shell quoting
// is hard. But then for consistency it also does Linux & macOS, even though
// those are trivial from YAML.
package main

import (
	"log"
	"os"

	"github.com/tailscale/gomodfs/testing/nfsmount/mount"
)

func main() {
	if os.Getenv("CI") != "true" {
		log.Fatalf("startgomodfs is only intended to be run in CI")
	}

	mount.Mount(2049)
}
