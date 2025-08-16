// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tailscale/gomodfs/store/gitstore"
)

var debugFUSE = flag.Bool("debug-fuse", false, "verbose FUSE debugging")

var hitNetwork = flag.Bool("run-network-tests", false, "run network tests")

func TestFilesystem(t *testing.T) {
	if !*hitNetwork {
		t.Skip("Skipping network tests; set --run-network-tests to run them")
	}

	type testEnv struct {
		*testing.T
		mnt string
	}

	type checker func(t testEnv) error
	pathContents := func(path, want string) checker {
		return func(t testEnv) error {
			got, err := os.ReadFile(filepath.Join(t.mnt, path))
			if err != nil {
				return err
			}
			if string(got) != want {
				return fmt.Errorf("unexpected content for %q: got %q, want %q", path, got, want)
			}
			return nil
		}
	}
	fileSize := func(path string, wantSize int64) checker {
		return func(t testEnv) error {
			fi, err := os.Stat(filepath.Join(t.mnt, path))
			if err != nil {
				return err
			}
			if fi.IsDir() {
				return fmt.Errorf("expected file %q to be a regular file, but it is a directory", path)
			}
			if fi.Size() != wantSize {
				return fmt.Errorf("unexpected size for %q: got %d, want %d", path, fi.Size(), wantSize)
			}
			return nil
		}
	}

	tests := []struct {
		name   string
		checks []checker
	}{
		{
			name: "hit-cache-dir-first",
			checks: []checker{
				pathContents(
					"cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.mod",
					"module go4.org/mem\n\ngo 1.14\n",
				),
				fileSize(
					"go4.org/mem@v0.0.0-20240501181205-ae6ca9944745/mem.go",
					12195,
				),
			},
		},
		{
			name: "hit-file-dir-first",
			checks: []checker{
				fileSize(
					"go4.org/mem@v0.0.0-20240501181205-ae6ca9944745/mem.go",
					12195,
				),
				pathContents(
					"cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.mod",
					"module go4.org/mem\n\ngo 1.14\n",
				),
			},
		},
		{
			name: "module-without-slash",
			checks: []checker{
				fileSize(
					"gocloud.dev@v0.20.0/blob/fileblob/example_test.go",
					2137,
				),
			},
		},
		{
			// This test is a regression test for a bug where this repo contains
			// a tree with a "testing" directory and "testing.md" file. Per
			// normal sorting rules, "testing" should come first in the tree
			// object, but git has an undocumented rule that tree entries
			// pointing to trees have an implicit final "/" at the end of their
			// name before sorting happens. Without that, git fsck will fail
			// and git index-pack --strict won't accept the pack file.
			name: "tree-not-sorted-error",
			checks: []checker{
				fileSize(
					"cache/download/google.golang.org/api/@v/v0.57.0.mod",
					661,
				),
			},
		},
		{
			name: "file-with-spaces", // whoops
			checks: []checker{
				fileSize(
					"github.com/docker/cli@v25.0.0+incompatible/cli/command/image/testdata/remove-command-success.Image Deleted and Untagged.golden",
					33,
				),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			goModCacheDir := t.TempDir()
			t.Logf("mount: %v", goModCacheDir)
			gitDir := t.TempDir()

			cmd := exec.Command("git", "init", gitDir)
			cmd.Dir = gitDir
			if err := cmd.Run(); err != nil {
				t.Fatalf("Failed to init git repo: %v", err)
			}

			store := &gitstore.Storage{GitRepo: gitDir}
			mfs := &FS{
				Store:   store,
				Verbose: true,
			}

			root := &moduleNameNode{
				fs: mfs,
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

			for i, check := range tt.checks {
				env := testEnv{
					T:   t,
					mnt: goModCacheDir,
					// TODO: stats on HTTP faults
				}
				if err := check(env); err != nil {
					t.Errorf("check[%d]: %v", i, err)
				}
			}
		})
	}

}
