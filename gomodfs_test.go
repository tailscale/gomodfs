package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tailscale/gomodfs/modgit"
)

var debugFUSE = flag.Bool("debug-fuse", false, "verbose FUSE debugging")

func TestGit(t *testing.T) {
	tmpMntDir := t.TempDir()
	t.Logf("mount: %v", tmpMntDir)

	d := &modgit.Downloader{GitRepo: "."}
	conf := &config{Git: d}

	curTree, err := exec.Command("git", "rev-parse", "HEAD^{tree}").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}

	root := &treeNode{
		conf: conf,
		tree: strings.TrimSpace(string(curTree)),
	}
	server, err := fs.Mount(tmpMntDir, root, &fs.Options{
		MountOptions: fuse.MountOptions{Debug: *debugFUSE},
	})
	if err != nil {
		log.Panic(err)
	}
	defer func() {
		err := server.Unmount()
		if err != nil {
			t.Errorf("Unmount error: %v", err)
		}
	}()
	didWait := make(chan struct{})
	go func() {
		defer close(didWait)
		server.Wait()
	}()

	err = filepath.Walk(tmpMntDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Logf("error walking into %q: %v", path, err)
			return err
		}
		rel, err := filepath.Rel(tmpMntDir, path)
		if err != nil {
			return fmt.Errorf("rel: %w", err)
		}
		t.Logf("walked: %v, %v", rel, fi.Mode())
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
}
