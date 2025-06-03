package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type moduleNameNode struct {
	fs.Inode
	paths []string

	didInit bool
}

type moduleRoot struct {
	fs.Inode
	modName string // "tailscale.com@v1.83.0-pre.0.20250508040513-5be6ff9b62ec"
}

func (n *moduleRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name != "info" {
		return nil, syscall.ENOENT
	}
	return n.NewInode(ctx, &memFile{
		contents: []byte("Module: " + n.modName + "\n"),
	}, fs.StableAttr{}), 0
}

// Ensure that we implement NodeOnAdder
var _ = (fs.NodeOnAdder)((*moduleNameNode)(nil))

// OnAdd is called on mounting the file system. Use it to populate
// the file system tree.
func (s *moduleNameNode) OnAdd(ctx context.Context) {
	s.didInit = true
}

// Node types should implement some file system operations, eg. Lookup
var _ = (fs.NodeLookuper)((*moduleNameNode)(nil))

var rxCapital = regexp.MustCompile(`\![a-z]`)

func (n *moduleNameNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if strings.Contains(name, "@") {
		modName := strings.Join(append(slices.Clone(n.paths), name), "/")
		modName = rxCapital.ReplaceAllStringFunc(modName, func(in string) string {
			return strings.ToUpper(in[1:])
		})
		in := n.Inode.NewInode(ctx, &moduleRoot{
			modName: modName,
		}, fs.StableAttr{
			Mode: fuse.S_IFDIR | 0755,
		})
		return in, 0

	}
	in := n.Inode.NewInode(ctx, &moduleNameNode{
		paths: append(slices.Clone(n.paths), name),
	}, fs.StableAttr{
		Mode: fuse.S_IFDIR | 0755,
	})
	return in, 0
}

type memFile struct {
	fs.Inode
	contents []byte
}

var _ = (fs.NodeOpener)((*memFile)(nil))
var _ = (fs.NodeReader)((*memFile)(nil))
var _ = (fs.NodeGetattrer)((*memFile)(nil))

func (f *memFile) Read(ctx context.Context, h fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	end := int(off) + len(dest)
	if end > len(f.contents) {
		end = len(f.contents)
	}
	return fuse.ReadResultData(f.contents[off:end]), 0
}

func (f *memFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags != 0 {
		log.Printf("Open with flags %x", flags)
		return nil, 0, syscall.EINVAL
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *memFile) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0644 | fuse.S_IFREG
	out.Size = uint64(len(f.contents))
	return 0
}

// This demonstrates how to build a file system in memory. The
// read/write logic for the file is provided by the MemRegularFile type.
func main() {
	mntDir := filepath.Join(os.Getenv("HOME"), "mnt-gomodfs")
	exec.Command("umount", mntDir).Run() // best effort

	if err := os.MkdirAll(mntDir, 0755); err != nil {
		log.Panicf("Failed to create mount directory %s: %v", mntDir, err)
	}

	root := &moduleNameNode{}
	server, err := fs.Mount(mntDir, root, &fs.Options{
		MountOptions: fuse.MountOptions{Debug: true},
	})
	if err != nil {
		log.Panic(err)
	}

	log.Printf("Mounted on %s", mntDir)
	log.Printf("Unmount by calling 'umount' (macOS) or 'fusermount -u' (Linux) with arg %s", mntDir)

	server.Wait()
}
