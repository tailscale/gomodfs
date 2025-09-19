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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
	flagMemLimitMB = flag.Int64("mem-limit-mb", 0, "how many megabytes (MiB) of memory gomodfs can use to store file contents in memory; 0 means to use a default")
)

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
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)
	st := stats.NewStatsWithRegistry(reg)
	gitStore := &gitstore.Storage{
		GitRepo: gitCache,
		Stats:   st,
	}
	mfs := &gomodfs.FS{
		Store:   gitStore,
		Stats:   st,
		Verbose: *verbose,
	}
	if *flagMemLimitMB != 0 {
		mfs.FileCacheSize = *flagMemLimitMB << 20
	}

	nfsHandler := mfs.NFSHandler()

	if *debugListen != "" {
		ln, err := net.Listen("tcp", *debugListen)
		if err != nil {
			log.Fatalf("Failed to listen on %s: %v", *debugListen, err)
		}
		log.Printf("Debug HTTP server listening on %s", *debugListen)

		mfs.RegisterMetrics(reg)

		metricsHandler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorLog: log.Default(),
		})

		hs := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.RequestURI == "/metrics" {
					metricsHandler.ServeHTTP(w, r)
				} else {
					mfs.ServeHTTP(w, r)
				}
			}),
		}
		go hs.Serve(ln)
	}

	var nfsListenAddr net.Addr
	if *flagNFS != "" {
		if *verbose {
			nfs.Log.SetLevel(nfs.TraceLevel)
		}
		ln, err := net.Listen("tcp", *flagNFS)
		if err != nil {
			log.Fatalf("Failed to listen on NFS port %s: %v", *flagNFS, err)
		}
		nfsListenAddr = ln.Addr()
		log.Printf("NFS server listening at %s", nfsListenAddr)
		if runtime.GOOS == "darwin" && mntDir == "" {
			port := ln.Addr().(*net.TCPAddr).Port
			log.Printf("To mount:\n\t mount -o port=%d,mountport=%d,vers=3,tcp,locallocks,soft -r -t nfs localhost:/ $HOME/mnt-gomodfs", port, port)
		}
		go nfs.Serve(ln, nfsHandler)
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
	} else if *flagNFS != "" {
		err = mfs.MountNFS(mntDir, nfsListenAddr)
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

	if mount != nil {
		mount.Wait()
	} else {
		select {}
	}
}
