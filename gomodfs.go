package main

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tailscale/gomodfs/modgit"
	"go4.org/mem"
)

type config struct {
	Git *modgit.Downloader // or nil to use default client

}

type moduleNameNode struct {
	fs.Inode
	paths []string
	conf  *config

	didInit bool
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

		res, err := n.conf.Git.Get(ctx, modName)
		if err != nil {
			log.Printf("Failed to get module %q: %v", modName, err)
			return nil, syscall.EIO
		}
		return n.NewInode(ctx, &treeNode{
			conf: n.conf,
			tree: res.Tree,
		}, fs.StableAttr{
			Mode: fuse.S_IFDIR | 0755,
		}), 0
	}
	in := n.Inode.NewInode(ctx, &moduleNameNode{
		conf:  n.conf,
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

type treeNode struct {
	fs.Inode
	conf *config
	tree string
}

// returns one of "blob", "tree", TODO-link.
//
// If ent doesn't exist in treeHash, it returns
// error os.ErrNotExist.
func gitType(dir, treeHash string, ent string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "cat-file", "-t", treeHash+":"+ent)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if mem.Contains(mem.B(out), mem.S("does not exist in")) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

var _ fs.NodeLookuper = (*treeNode)(nil)
var _ fs.NodeReaddirer = (*treeNode)(nil)

func (n *treeNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	typ, err := gitType(n.conf.Git.GitRepo, n.tree, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, syscall.ENOENT
		}
		log.Printf("Failed to get type of %q in tree %q: %v", name, n.tree, err)
		return nil, syscall.EIO
	}
	switch typ {
	case "blob":
		cmd := exec.Command("git", "-C", n.conf.Git.GitRepo, "cat-file", "-p", n.tree+":"+name)
		outData, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("Failed to get contents of %q in tree %q: %v", name, n.tree, err)
			return nil, syscall.EIO
		}
		in := n.Inode.NewInode(ctx, &memFile{
			contents: outData,
		}, fs.StableAttr{
			Mode: fuse.S_IFREG | 0644, // TODO: proper mode
		})
		return in, 0
	case "tree":
		cmd := exec.Command("git", "-C", n.conf.Git.GitRepo, "rev-parse", n.tree+":"+name)
		outData, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("Failed to get tree hash of %q in tree %q: %v", name, n.tree, err)
			return nil, syscall.EIO
		}
		return n.Inode.NewInode(ctx, &treeNode{
			conf: n.conf,
			tree: strings.TrimSpace(string(outData)),
		}, fs.StableAttr{
			Mode: fuse.S_IFDIR | 0755,
		}), 0
	}

	log.Printf("Unknown type %q for %q in tree %q", typ, name, n.tree)
	return nil, syscall.ENOENT
}

func (n *treeNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	cmd := exec.Command("git", "-C", n.conf.Git.GitRepo, "ls-tree", n.tree)
	outData, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to list tree %q: %v", n.tree, err)
		return nil, syscall.EIO
	}

	var ents []fuse.DirEntry
	bs := bufio.NewScanner(bytes.NewReader(outData))
	for bs.Scan() {
		f := strings.Fields(bs.Text())
		if len(f) != 4 {
			log.Printf("Unexpected ls-tree output: %q", bs.Text())
			continue
		}
		modeStr, name := f[0], f[3]
		// Skip the type in f[1] ("blob", "tree", etc)
		// Skip the hash in f[2]
		mode, err := strconv.ParseUint(modeStr, 10, 32)
		if err != nil {
			log.Printf("Failed to parse mode %q in ls-tree output: %v", modeStr, err)
			return nil, syscall.EIO
		}
		ents = append(ents, fuse.DirEntry{
			Name: name,
			Mode: uint32(mode),
			Off:  uint64(len(ents)),
		})

	}
	if err := bs.Err(); err != nil {
		log.Printf("Failed to parse ls-tree output: %v", err)
		return nil, syscall.EIO
	}
	return &treeDirStream{ents: ents}, 0
}

type treeDirStream struct {
	ents []fuse.DirEntry
}

func (s *treeDirStream) HasNext() bool { return len(s.ents) > 0 }
func (s *treeDirStream) Close()        {}
func (s *treeDirStream) Next() (fuse.DirEntry, syscall.Errno) {
	ent := s.ents[0]
	s.ents = s.ents[1:]
	return ent, 0
}

// This demonstrates how to build a file system in memory. The
// read/write logic for the file is provided by the MemRegularFile type.
func main() {
	gitCache := filepath.Join(os.Getenv("HOME"), ".cache", "gomodfs")
	if err := os.MkdirAll(gitCache, 0755); err != nil {
		log.Panicf("Failed to create git cache directory %s: %v", gitCache, err)
	}
	cmd := exec.Command("git", "init", gitCache)
	cmd.Dir = gitCache
	cmd.Run() // best effort

	d := &modgit.Downloader{GitRepo: gitCache}

	mntDir := filepath.Join(os.Getenv("HOME"), "mnt-gomodfs")
	exec.Command("umount", mntDir).Run() // best effort

	if err := os.MkdirAll(mntDir, 0755); err != nil {
		log.Panicf("Failed to create mount directory %s: %v", mntDir, err)
	}

	conf := &config{
		Git: d,
	}

	root := &moduleNameNode{
		conf: conf,
	}
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
