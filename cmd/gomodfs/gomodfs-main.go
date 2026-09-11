// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The gomodfs server is a virtual file system (FUSE or WebDAV) that implements
// a read-only GOMODCACHE filesystem that pretends that all modules are accessible,
// downloading them on demand as needed.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bradfitz/parentdeath"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tailscale/gomodfs"
	"github.com/tailscale/gomodfs/gitrepo"
	"github.com/tailscale/gomodfs/nfsexports"
	"github.com/tailscale/gomodfs/stats"
	"github.com/tailscale/gomodfs/store/gitstore"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
)

var (
	debugListen    = flag.String("http-debug", "", "if set, listen on this address for a debug HTTP server")
	verbose        = flag.Bool("verbose", false, "enable verbose logging")
	useWebDAV      = flag.Bool("webdav", false, "use WebDAV instead of FUSE (useful on macOS w/o kernel extensions allowed)")
	flagNFS        = flag.String("nfs", "", "if set, listen on this port for NFS requests")
	flagMountPoint = flag.String("mount", "", "if set, mount the filesystem at this path")
	flagMemLimitMB = flag.Int64("mem-limit-mb", 0, "how many megabytes (MiB) of memory gomodfs can use to store file contents in memory; 0 means to use a default")
	flagRepo       = flag.String("repo", "", "experimental: Git repository URL to export as /repos/<owner>/<repo>/<commit> over NFS. The <owner> and <repo> are inferred from the last 2 slash-separated segments")
	flagCommit     = flag.String("commit", "", "experimental: full commit SHA to export from -repo")
	portmapper     = flag.Bool("portmapper", false, "if set, run rpcbind portmapper on TCP+UDP port 111 (needed for Windows NFS clients). For NFS mode only")
	flagWinFSP     = flag.Bool("winfsp", false, "if set, use WinFSP on Windows")

	// TODO: ideally auto-detect and remove this flag.
	flagNFSForWindows = flag.Bool("nfs-for-windows-clients", runtime.GOOS == "windows", "if set, alter NFS server behavior for Windows clients (TODO: ideally auto-detect and remove this flag)")
)

func repositoryName(remote string) (string, error) {
	u, err := url.Parse(remote)
	if err != nil {
		return "", fmt.Errorf("invalid -repo URL %q: %w", remote, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid -repo URL %q; want a path ending in owner/repo", remote)
	}
	parts[len(parts)-1] = strings.TrimSuffix(parts[len(parts)-1], ".git")
	return strings.Join(parts[len(parts)-2:], "/"), nil
}

func main() {
	flag.Parse()

	parentdeath.Monitor(func() {
		log.Printf("gomodfs: parent process died, exiting")
		os.Exit(0)
	})

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("os.UserHomeDir: %v", err)
	}
	gitCache := filepath.Join(homeDir, ".cache", "gomodfs")
	if err := os.MkdirAll(gitCache, 0755); err != nil {
		log.Panicf("Failed to create git cache directory %s: %v", gitCache, err)
	}
	cmd := exec.Command("git", "init", gitCache)
	cmd.Dir = gitCache
	cmd.Run() // best effort

	mntDir := *flagMountPoint
	if mntDir != "" && runtime.GOOS != "windows" {
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

	if *portmapper {
		if err := startPortmapper(); err != nil {
			log.Fatalf("Failed to start portmapper: %v", err)
		}
	}

	var nfsHandler nfs.Handler = mfs.NFSHandler()
	if *flagRepo != "" || *flagCommit != "" {
		if *flagNFS == "" || *flagRepo == "" || *flagCommit == "" {
			log.Fatal("-repo and -commit require each other and -nfs")
		}
		name, err := repositoryName(*flagRepo)
		if err != nil {
			log.Fatal(err)
		}
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("os.UserHomeDir: %v", err)
		}
		manager := gitrepo.NewManager(filepath.Join(homeDir, ".cache", "gomodfs-repos"), map[string]gitrepo.Config{
			name: {
				RemoteURL: *flagRepo,
			},
		})
		manager.RegisterMetrics(reg)
		gfs, err := gitrepo.NewNFSHandler(manager, 0, 0)
		if err != nil {
			log.Fatal(err)
		}
		release, err := gfs.Acquire(context.Background(), name, *flagCommit)
		if err != nil {
			log.Fatal(err)
		}
		defer release()
		nfsHandler = nfsexports.New(mfs, gfs)
	}

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

		debugMux := http.NewServeMux()
		debugMux.Handle("/metrics", metricsHandler)
		debugMux.HandleFunc("/status.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(mfs.StatusJSON())
		})
		debugMux.Handle("/", mfs)
		debugMux.HandleFunc("/debug/pprof/", pprof.Index)
		debugMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		debugMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		debugMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		debugMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		hs := &http.Server{
			Handler: debugMux,
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
		nfsSrv := &nfs.Server{
			Handler:           nfsHandler,
			ForWindowsClients: *flagNFSForWindows,
		}
		go nfsSrv.Serve(ln)
	}

	if runtime.GOOS == "windows" && *flagWinFSP && mntDir == "" {
		mntDir = "M:"
	}

	if mntDir == "" {
		log.Printf("Not mounting filesystem, use --mount flag to specify mount point")
		select {}
	}

	var mount gomodfs.MountRunner
	if *useWebDAV {
		mount, err = mfs.MountWebDAV(mntDir, &gomodfs.MountOpts{
			Debug: *verbose,
		})
	} else if *flagWinFSP {
		mount, err = mfs.MountWinFSP(mntDir)
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
	if runtime.GOOS != "windows" {
		log.Printf("Unmount by calling 'umount' (macOS) or 'fusermount -u' (Linux) with arg %s", mntDir)
	}

	if mount != nil {
		mount.Wait()
	} else {
		select {}
	}
}
