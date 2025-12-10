// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The startgomodfs binary is used in CI tests to start a gomodfs server on
// Windows, because Powershell-in-YAML-in-Github-Actions with shell quoting
// is hard. But then for consistency it also does Linux & macOS, even though
// those are trivial from YAML.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	if os.Getenv("CI") != "true" {
		log.Fatalf("startgomodfs is only intended to be run in CI")
	}

	var cmd *exec.Cmd
	var mntDir string
	var machineIP = "127.0.0.1"

	useTempMnt := func() {
		mntDir = filepath.Join(os.TempDir(), "gomodfs-mnt")
		if err := os.MkdirAll(mntDir, 0755); err != nil {
			log.Fatalf("creating mount dir %s: %v", mntDir, err)
		}

		ifs, err := net.Interfaces()
		if err != nil {
			log.Fatalf("net.Interfaces: %v", err)
		}
		for _, ifi := range ifs {
			if ifi.Flags&net.FlagLoopback != 0 ||
				ifi.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, err := ifi.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				ipStr := addr.String()
				if strings.Contains(ipStr, ":") {
					continue
				}
				if s, _, ok := strings.Cut(ipStr, "/"); ok {
					machineIP = s
					log.Printf("using machine IP %q for NFS mount", machineIP)
					return
				}
			}
		}
	}

	checkUp := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:2049")
		if err != nil {
			return fmt.Errorf("NFS dial error: %v", err)
		}
		conn.Close()
		return nil
	}
	if err := checkUp(); err != nil {
		log.Fatalf("gomodfs should already be running, but got: %v", err)
	}

	switch runtime.GOOS {
	case "windows":
		mntDir = "Z:"
		cmd = exec.Command("mount.exe",
			"-o", "anon",
			"-o", "mtype=hard",
			"-o", "casesensitive=yes",
			// TODO: add "-o", "nolock" once we send a fix to cmd/go
			// to upstream Go to not require RLock to pass on file-readonly
			// filesystems.
			`\\127.0.0.1\modfs`,
			mntDir)
	case "linux":
		useTempMnt()
		cmd = exec.Command("sudo", "/usr/bin/mount",
			"-t", "nfs",
			"-o", linuxNFSMountOpts(2049),
			machineIP+":/gomodfs",
			mntDir,
		)
	case "darwin":
		useTempMnt()
		cmd = exec.Command("/sbin/mount",
			"-t", "nfs",
			"-o", darwinNFSMountOpts(2049),
			machineIP+":/gomodfs",
			mntDir,
		)
	default:
		log.Fatalf("unsupported OS %q", runtime.GOOS)
	}

	log.Printf("running mount command: %q %q ...", cmd.Path, cmd.Args)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("gomodfs: failed to run mount command %q %q: %v, output: %s", cmd.Path, cmd.Args, err, out)
	}

	log.Printf("mounted; GOOS=%q, mount point=%q", runtime.GOOS, mntDir)
	if runtime.GOOS == "linux" {
		proc, _ := os.ReadFile("/proc/mounts")
		log.Printf("/proc/mounts has:\n%s\n# ======\n", proc)
	}

	timer := time.AfterFunc(10*time.Second, func() {
		const logPath = "./gomodfs.log"
		logData, err := os.ReadFile(logPath)
		if err != nil {
			log.Printf("after 10s: reading log file %s: %v", logPath, err)
			return
		}
		const max = 512 << 10
		if len(logData) > max {
			logData = logData[:max]
		}
		log.Printf("after 10s: contents of %s:\n%s\n# ======\n", logPath, logData)
	})
	time.AfterFunc(30*time.Second, func() {
		panic("timeout")
	})

	log.Printf("mounted. reading directory...")
	dir, err := os.ReadDir(mntDir)
	timer.Stop()
	if err != nil {
		log.Fatalf("reading mount dir %s: %v", mntDir, err)
	}
	if len(dir) == 0 {
		log.Fatalf("mount dir %s appears empty; mount failed?", mntDir)
	}

	var names []string
	for _, d := range dir {
		names = append(names, d.Name())
	}

	log.Printf("gomodfs mounted successfully at %s; contents: %q", mntDir, names)

	if e := os.Getenv("GITHUB_ENV"); e != "" {
		mcDir := mntDir

		// On Windows, ensure the path has a trailing slash
		// and isn't just a drive letter like "Z:".
		if runtime.GOOS == "windows" && strings.HasSuffix(mcDir, ":") {
			mcDir += "\\"
		}
		err := appendFile(e, fmt.Appendf(nil, "GOMODCACHE=%s\n", mcDir), 0644)
		if err != nil {
			log.Fatalf("writing GOMODFS to GITHUB_ENV file %q: %v", e, err)
		}
		log.Printf("set env GOMODCACHE=%s", mcDir)
	}
}

func appendFile(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// TODO(bradfitz): de-dup these from gomodfs.go?

func linuxNFSMountOpts(nfsPort int) string {
	opts := []string{
		// port is port at which NFS service should be available
		// https://datatracker.ietf.org/doc/html/rfc1813#section-3.
		"port=" + fmt.Sprint(nfsPort),
		// mountport is port at which the mount RPC service should be available
		// https://datatracker.ietf.org/doc/html/rfc1813#section-5.2.
		"mountport=" + fmt.Sprint(nfsPort),
		// version is NFS version. go-nfs implements NFSv3.
		"vers=3",
		"tcp",
		"local_lock=all", // required to pacify cmd/go file locking

		// timeout for NFS operations in deciseconds.
		// 1800 = 3 minutes.
		"timeo=1800",
		// No need for reserved ports; gomodfs doesn't require them.
		"noresvport",
		// Create a readonly volume.
		"ro",

		// Disable the NFS_ACL sidecar protocol probe (RPC 100227).
		// Just quiets some log spam; gomodfs doesn't support ACLs.
		"noacl",
	}
	return strings.Join(opts, ",")
}

func darwinNFSMountOpts(nfsPort int) string {
	opts := []string{
		// port is port at which NFS service should be available
		// https://datatracker.ietf.org/doc/html/rfc1813#section-3.
		"port=" + fmt.Sprint(nfsPort),
		// mountport is port at which the mount RPC service should be available
		// https://datatracker.ietf.org/doc/html/rfc1813#section-5.2.
		"mountport=" + fmt.Sprint(nfsPort),
		// version is NFS version. go-nfs implements NFSv3.
		"vers=3",
		"tcp",
		"locallocks", // required to pacify cmd/go file locking

		// timeout for NFS operations in deciseconds.
		// 1800 = 3 minutes.
		"timeo=1800",
		// Create a readonly volume.
		"rdonly",
	}
	return strings.Join(opts, ",")
}
