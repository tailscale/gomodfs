// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The startgomodfs binary is used in CI tests to start a gomodfs server on
// Windows, because Powershell-in-YAML-in-Github-Actions with shell quoting
// is hard. But then for consistency it also does Linux & macOS, even though
// those are trivial from YAML.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"syscall"
	"time"
)

const (
	binPath = "./gomodfs.exe" // we use '.exe' in CI even on non-Windows; why not?
	logPath = "./gomodfs.log"
	pidPath = "./gomodfs.pid"
)

var (
	useWinFSP = flag.Bool("winfsp", false, "on Windows, use WinFSP to mount the filesystem")
)

func main() {
	flag.Parse()
	if os.Getenv("CI") != "true" {
		log.Fatalf("startgomodfs is only intended to be run in CI")
	}

	f, err := os.Create(logPath)
	if err != nil {
		log.Fatalf("creating log file: %v", err)
	}

	var cmd *exec.Cmd
	if *useWinFSP && runtime.GOOS == "windows" {
		cmd = exec.Command(binPath, "-http-debug=127.0.0.1:8080", "-mount=M:", "-winfsp", "-verbose")
	} else {
		cmd = exec.Command(binPath, "-http-debug=127.0.0.1:8080", "-mount=", "-nfs=:2049", "-verbose")
		if runtime.GOOS == "windows" {
			cmd.Args = append(cmd.Args, "-portmapper")
		}
	}

	cmd.Stdout = f
	cmd.Stderr = f
	if runtime.GOOS != "windows" {
		attr := &syscall.SysProcAttr{}
		// Use reflect so this compiles on Windows where that
		// field doesn't exist, without needing build tags.
		reflect.ValueOf(attr).Elem().FieldByName("Setsid").SetBool(true)
		cmd.SysProcAttr = attr
	}

	// Before we start, verify that the NFS port is not already in use.
	if !*useWinFSP {
		if err := port2049Up(); err == nil {
			log.Fatalf("NFS port 2049 is already in use; is gomodfs already running?")
		}
	}

	if err := cmd.Start(); err != nil {
		pwd, _ := os.Getwd()
		log.Fatalf("starting %s in %s: %v", binPath, pwd, err)
	}
	pid := cmd.Process.Pid
	log.Printf("started %s with pid %d", binPath, pid)

	if err := os.WriteFile(pidPath, fmt.Appendf(nil, "%d", pid), 0644); err != nil {
		log.Fatalf("writing pid file: %v", err)
	}

	for range 10 {
		err = checkUp()
		if err == nil {
			log.Printf("gomodfs is up; success")
			return
		}
		log.Printf("gomodfs not up yet: %v", err)
		time.Sleep(time.Second)
	}
	log.Fatalf("gomodfs did not start up in time: %v", err)
}

func port2049Up() error {
	var d net.Dialer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:2049")
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// checkUp checks whether the gomodfs server is up by querying its /metrics
// HTTP endpoint. It also checks that port 2049 is up.
func checkUp() error {
	log, _ := os.ReadFile(logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "HEAD", "http://127.0.0.1:8080/metrics", nil)
	if err != nil {
		return fmt.Errorf("creating HTTP request: %v; log: %s", err, log)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP error: %v; log: %s", err, log)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %v; log: %s", resp.Status, log)
	}

	if *useWinFSP {
		// No NFS server to check.
		return nil
	}
	return port2049Up()
}
