// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The gomodfs server is a virtual file system (FUSE or WebDAV) that implements
// a read-only GOMODCACHE filesystem that pretends that all modules are accessible,
// downloading them on demand as needed.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/tailscale/gomodfs"
	"github.com/tailscale/gomodfs/stats"
	"github.com/tailscale/gomodfs/store/gitstore"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
)

var (
	debugListen    = flag.String("http-debug", "", "if set, listen on this address for a debug HTTP server")
	verbose        = flag.Bool("verbose", false, "enable verbose logging")
	useWebDAV      = flag.Bool("webdav", false, "use WebDAV instead of FUSE (useful on macOS w/o kernel extensions allowed)")
	flagNFS        = flag.String("nfs", "", "if set, listen on this port for NFS requests")
	flagMountPoint = flag.String("mount", filepath.Join(os.Getenv("HOME"), "mnt-gomodfs"), "if set, mount the filesystem at this path")
)

// This demonstrates how to build a file system in memory. The
// read/write logic for the file is provided by the MemRegularFile type.
func main() {
	flag.Parse()

	gitCache := filepath.Join(os.Getenv("HOME"), ".cache", "gomodfs")
	if err := os.MkdirAll(gitCache, 0755); err != nil {
		log.Panicf("Failed to create git cache directory %s: %v", gitCache, err)
	}
	cmd := exec.Command("git", "init", gitCache)
	cmd.Dir = gitCache
	cmd.Run() // best effort

	mntDir := *flagMountPoint
	if mntDir != "" {
		exec.Command("umount", mntDir).Run() // best effort
		if os.Getenv("GOOS") == "darwin" {
			exec.Command("diskutil", "unmount", "force", mntDir).Run() // best effort
		}
		if err := os.MkdirAll(mntDir, 0755); err != nil {
			log.Panicf("Failed to create mount directory %s: %v", mntDir, err)
		}
	}

	st := &stats.Stats{}
	gitStore := &gitstore.Storage{
		GitRepo: gitCache,
		Stats:   st,
	}
	mfs := &gomodfs.FS{
		Git:   gitStore,
		Store: gitStore,
		Stats: st,
	}

	if *debugListen != "" {
		ln, err := net.Listen("tcp", *debugListen)
		if err != nil {
			log.Fatalf("Failed to listen on %s: %v", *debugListen, err)
		}
		log.Printf("Debug HTTP server listening on %s", *debugListen)
		hs := &http.Server{
			Handler: mfs,
		}
		go hs.Serve(ln)
	}

	if *flagNFS != "" {
		if *verbose {
			nfs.Log.SetLevel(nfs.TraceLevel)
		}
		ln, err := net.Listen("tcp", *flagNFS)
		if err != nil {
			log.Fatalf("Failed to listen on NFS port %s: %v", *flagNFS, err)
		}
		log.Printf("NFS server listening at %s", ln.Addr())
		if runtime.GOOS == "darwin" {
			port := ln.Addr().(*net.TCPAddr).Port
			log.Printf("To mount:\n\t mount -o port=%d,mountport=%d -r -t nfs localhost:/ $HOME/mnt-gomodfs", port, port)
		}
		go nfs.Serve(ln, mfs.NFSHandler())
	}

	if mntDir == "" {
		log.Printf("Not mounting filesystem, use --mount flag to specify mount point")
		select {}
	}

	var err error
	var mount gomodfs.FileServer
	if *useWebDAV {
		mount, err = mfs.MountWebDAV(mntDir, &gomodfs.MountOpts{
			Debug: *verbose,
		})
	} else {
		mount, err = mfs.MountFUSE(mntDir, &gomodfs.MountOpts{
			Debug: *verbose,
		})
	}
	if err != nil {
		log.Fatalf("Failed to mount filesystem: %v", err)
	}

	log.Printf("Mounted on %s", mntDir)
	log.Printf("Unmount by calling 'umount' (macOS) or 'fusermount -u' (Linux) with arg %s", mntDir)

	mount.Wait()
}
