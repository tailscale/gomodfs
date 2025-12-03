// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The startgomodfs binary is used in CI tests to start a gomodfs server on
// Windows, because Powershell-in-YAML-in-Github-Actions with shell quoting
// is hard. But then for consistency it also does Linux & macOS, even though
// those are trivial from YAML.
package main

import (
	"fmt"
	"log"
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

func main() {
	f, err := os.Create(logPath)
	if err != nil {
		log.Fatalf("creating log file: %v", err)
	}
	cmd := exec.Command(binPath, "-http-debug=127.0.0.1:8080", "-mount=", "-nfs=127.0.0.1:2049")
	cmd.Stdout = f
	cmd.Stderr = f
	if runtime.GOOS != "windows" {
		attr := &syscall.SysProcAttr{}
		// Use reflect so this compiles on Windows where that
		// field doesn't exist, without needing build tags.
		reflect.ValueOf(attr).Elem().FieldByName("Setsid").SetBool(true)
		cmd.SysProcAttr = attr
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

	checkUp := func() error {
		log, _ := os.ReadFile(logPath)
		resp, err := http.DefaultClient.Head("http://127.0.0.1:8080/metrics")
		if err != nil {
			return fmt.Errorf("HTTP error: %v; log: %s", err, log)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %v; log: %s", resp.Status, log)
		}
		return nil
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
