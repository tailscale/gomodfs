package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	curTree, err := exec.Command("git", "rev-parse", "HEAD:testdata").CombinedOutput()
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

	want := walk(t, "testdata")
	got := walk(t, tmpMntDir)
	if got != want {
		t.Fatalf("walk mismatch\n\nGOT:\n%s\nWANT:\n%s", got, want)
	}
	t.Logf("got:\n%s", got)
}

func walk(t testing.TB, dir string) string {
	t.Helper()
	var buf bytes.Buffer
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Logf("error walking into %q: %v", path, err)
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel: %w", err)
		}
		fmt.Fprintf(&buf, "%s", rel)
		switch {
		case fi.Mode().IsDir():
			fmt.Fprintf(&buf, "/, %v\n", fi.Mode())
		case fi.Mode().IsRegular():
			v, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&buf, ", %v, content %q\n", fi.Mode(), v)
		case fi.Mode().Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&buf, " => %q\n", target)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return buf.String()
}

func TestGoModules(t *testing.T) {

	type pathAndContents struct {
		path string
		want string
	}

	tests := []struct {
		name   string
		checks []pathAndContents
	}{
		{
			name: "cache-mod-first",
			checks: []pathAndContents{
				{
					path: "cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.mod",
					want: "module go4.org/mem\n\ngo 1.14\n",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			goModCacheDir := t.TempDir()
			t.Logf("mount: %v", goModCacheDir)

			d := &modgit.Downloader{GitRepo: "."}
			conf := &config{Git: d}

			root := &moduleNameNode{
				conf: conf,
			}
			server, err := fs.Mount(goModCacheDir, root, &fs.Options{
				MountOptions: fuse.MountOptions{Debug: *debugFUSE},
			})
			if err != nil {
				t.Fatalf("Mount: %v", err)
			}
			defer func() {
				err := server.Unmount()
				if err != nil {
					t.Errorf("Unmount error: %v", err)
				}
				if t.Failed() {
					t.Logf("Sleeping for 5m... to allow inspection of %s", goModCacheDir)
					time.Sleep(5 * time.Minute)
				}
			}()
			didWait := make(chan struct{})
			go func() {
				defer close(didWait)
				server.Wait()
			}()

			for _, check := range tt.checks {
				got, err := os.ReadFile(filepath.Join(goModCacheDir, check.path))
				if err != nil {
					t.Fatalf("ReadFile(%q) failed: %v", check.path, err)
				}
				if string(got) != check.want {
					t.Errorf("unexpected content for %q: got %q, want %q", check.path, got, check.want)
				}
			}
		})
	}

}
