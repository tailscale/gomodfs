package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tailscale/gomodfs/modgit"
	"go4.org/mem"
	"golang.org/x/mod/module"
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

func (n *moduleNameNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if len(n.paths) == 0 && name == "cache" {
		return n.NewInode(ctx, &cacheRootNode{
			conf: n.conf,
		}, fs.StableAttr{
			Mode: fuse.S_IFDIR | 0755,
		}), 0
	}
	if strings.Contains(name, "@") {
		finalFrag, ver, _ := strings.Cut(name, "@")
		escName := strings.Join(n.paths, "/") + "/" + finalFrag
		modName, err := module.UnescapePath(escName)
		if err != nil {
			log.Printf("Failed to unescape module name %q: %v", escName, err)
			return nil, syscall.EIO
		}
		res, err := n.conf.Git.Get(ctx, modName+"@"+ver)
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
	mode     uint32
}

var (
	_ = (fs.NodeOpener)((*memFile)(nil))
	_ = (fs.NodeReader)((*memFile)(nil))
	_ = (fs.NodeGetattrer)((*memFile)(nil))
)

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
	out.Mode = fuse.S_IFREG | (f.mode & 0o777)
	out.Size = uint64(len(f.contents))
	return 0
}

type symLink struct {
	fs.Inode
	contents []byte // contents of symlink
	mode     uint32
}

var _ = (fs.NodeGetattrer)((*symLink)(nil))
var _ = (fs.NodeReadlinker)((*symLink)(nil))

func (f *symLink) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFLNK | (f.mode & 0o777)
	return 0
}

func (f *symLink) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	return f.contents, 0
}

type treeNode struct {
	fs.Inode
	conf *config
	tree string

	mu       sync.Mutex
	treeEnt  map[string]*gitTreeEnt // non-nil once initialized
	treeEnts []*gitTreeEnt          // non-nil once initialized
}

type gitTreeEnt struct {
	mode    uint32
	gitType string // "blob" (files + symlinks), "tree"
	ref     string
	name    string // base name

	// even a symlink is:
	// 120000 blob 996f1789ff67c0e3f69ef5933a55d54c5d0e9954    some-symlink
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

func (n *treeNode) initEnts() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.treeEnt != nil {
		return nil
	}
	cmd := exec.Command("git", "-C", n.conf.Git.GitRepo, "ls-tree", n.tree)
	outData, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to list tree %q: %w", n.tree, err)
	}

	gitType := func(t mem.RO) string {
		if t.EqualString("blob") {
			return "blob"
		}
		if t.EqualString("tree") {
			return "tree"
		}
		return t.StringCopy()
	}

	ents := []*gitTreeEnt{} // always non-nil
	bs := bufio.NewScanner(bytes.NewReader(outData))
	var f []mem.RO
	for bs.Scan() {
		f = mem.AppendFields(f[:0], mem.B(bs.Bytes()))
		if len(f) != 4 {
			return fmt.Errorf("unexpected ls-tree output: %q", bs.Text())
		}
		mode, _ := mem.ParseUint(f[0], 8, 32)
		ents = append(ents, &gitTreeEnt{
			mode:    uint32(mode),
			gitType: gitType(f[1]),
			ref:     f[2].StringCopy(),
			name:    f[3].StringCopy(),
		})
	}

	n.treeEnts = ents
	n.treeEnt = map[string]*gitTreeEnt{}
	for _, ge := range ents {
		n.treeEnt[ge.name] = ge
	}
	return nil
}

func (n *treeNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if err := n.initEnts(); err != nil {
		log.Printf("Lookup(%q) initEnts error: %v", name, err)
		return nil, syscall.EIO
	}
	ge, ok := n.treeEnt[name]
	if !ok {
		return nil, syscall.ENOENT
	}
	switch ge.gitType {
	case "blob":
		cmd := exec.Command("git", "-C", n.conf.Git.GitRepo, "cat-file", "-p", ge.ref)
		outData, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("Failed to get contents of %q in tree %q: %v", name, n.tree, err)
			return nil, syscall.EIO
		}
		var node fs.InodeEmbedder
		var mode = ge.mode & 0o777

		const typBits = fuse.S_IFDIR | fuse.S_IFREG | fuse.S_IFLNK | fuse.S_IFIFO

		if ge.mode&typBits == fuse.S_IFLNK {
			mode |= fuse.S_IFLNK
			node = &symLink{
				mode:     ge.mode,
				contents: outData,
			}
		} else {
			mode |= fuse.S_IFREG
			node = &memFile{
				mode:     ge.mode,
				contents: outData,
			}
		}
		return n.Inode.NewInode(ctx, node, fs.StableAttr{Mode: mode}), 0
	case "tree":
		return n.Inode.NewInode(ctx, &treeNode{
			conf: n.conf,
			tree: ge.ref,
		}, fs.StableAttr{
			Mode: fuse.S_IFDIR | 0755,
		}), 0
	}

	log.Printf("Unknown type %q for %q in tree %q", ge.gitType, name, n.tree)
	return nil, syscall.ENOENT
}

func (n *treeNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if err := n.initEnts(); err != nil {
		log.Printf("Readdir(%v) initEnts error: %v", n.tree, err)
		return nil, syscall.EIO
	}
	var ents []fuse.DirEntry
	for _, ge := range n.treeEnts {
		ents = append(ents, fuse.DirEntry{
			Name: ge.name,
			Mode: ge.mode,
			Off:  uint64(len(ents)),
		})
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

// cacheRootNode is the $GOMODCACHE/cache directory, containing
// just a "download" directory within it.
type cacheRootNode struct {
	fs.Inode
	conf *config
}

var (
	_ fs.NodeLookuper = (*cacheRootNode)(nil)
)

func (n *cacheRootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name != "download" {
		log.Printf("Lookup(%q) in cache root, but only 'download' is allowed", name)
		return nil, syscall.ENOENT
	}
	in := n.NewInode(ctx, &cacheDownloadNode{
		conf: n.conf,
		segs: nil, // root
	}, fs.StableAttr{
		Mode: fuse.S_IFDIR | 0755,
	})
	return in, 0
}

// cacheDownloadNode is a path-segment-building node at or under the
// $GOMODCACHE/cache/download directory, containing the downloaded modules. It
// ends when it finds a "/@v/" segment, in which case its module field is set.
type cacheDownloadNode struct {
	fs.Inode
	conf *config

	segs   []string // path segments, e.g. ["!microsoft", "go-winio"] (empty if module is set)
	module string   // if non-empty, the module name unescaped, e.g. "Microsoft.com/go-winio" (in the /@v/ directory)
}

func netSlurp(ctx0 context.Context, urlStr string) (ret []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx0, 30*time.Second)
	defer cancel()

	defer func() {
		if err != nil && ctx0.Err() == nil {
			log.Printf("netSlurp(%q) failed: %v", urlStr, err)
		}
		if err == nil {
			log.Printf("netSlurp(%q) succeeded; %d bytes", urlStr, len(ret))
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %q: %w", urlStr, err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download %q: %w", urlStr, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download %q: %s", urlStr, res.Status)
	}
	return io.ReadAll(res.Body)
}

var (
	_ fs.NodeLookuper = (*cacheDownloadNode)(nil)
)

func (n *cacheDownloadNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.module != "" {
		return n.lookupUnderModule(ctx, name, out)
	}

	if name == "@v" {
		unescaped, err := module.UnescapePath(strings.Join(n.segs, "/"))
		if err != nil {
			log.Printf("Failed to unescape module name %q: %v", n.segs, err)
			return nil, syscall.EIO
		}
		in := n.NewInode(ctx, &cacheDownloadNode{
			conf:   n.conf,
			module: unescaped,
		}, fs.StableAttr{
			Mode: fuse.S_IFDIR | 0755,
		})
		return in, 0
	}

	in := n.NewInode(ctx, &cacheDownloadNode{
		conf: n.conf,
		segs: append(n.segs[:len(n.segs):len(n.segs)], name),
	}, fs.StableAttr{
		Mode: fuse.S_IFDIR | 0755,
	})
	return in, 0
}

// lookupUnderModule is Lookup for a cacheDownloadNode that's hit the /@v/
// directory, meaning it has a module name set.
func (n *cacheDownloadNode) lookupUnderModule(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	// We are in the /@v/ directory, so we should return a file. e.g. one of these:
	// /tmp/dl/cache/download/github.com/!microsoft/go-winio/@v/v0.6.2.info
	// /tmp/dl/cache/download/github.com/!microsoft/go-winio/@v/v0.6.2.mod
	// /tmp/dl/cache/download/github.com/!microsoft/go-winio/@v/v0.6.2.ziphash

	if strings.HasSuffix(name, ".partial") {
		return nil, syscall.ENOENT
	}

	log.Printf("XXX Lookup(%q) in module %q", name, n.module)
	escPath, err := module.EscapePath(n.module)
	if err != nil {
		log.Printf("Failed to escape module name %q: %v", n.module, err)
		return nil, syscall.EIO
	}

	if strings.HasSuffix(name, ".mod") || strings.HasSuffix(name, ".info") {
		slurp, err := netSlurp(ctx, fmt.Sprintf("https://proxy.golang.org/%s/@v/%s", escPath, name))
		if err != nil {
			return nil, syscall.EIO
		}
		in := n.NewInode(ctx, &memFile{
			contents: slurp,
			mode:     0644,
		}, fs.StableAttr{
			Mode: fuse.S_IFREG | 0644,
		})
		return in, 0
	}

	if strings.HasSuffix(name, ".ziphash") {
		d := n.conf.Git
		if d == nil {
			log.Printf("No git downloader configured, cannot fetch ziphash for %q", n.module)
		}
		version := strings.TrimSuffix(name, ".ziphash") // "v0.0.0-20240501181205-ae6ca9944745"
		zipHash, err := d.GetZipHash(escPath, version)
		if err != nil {
			log.Printf("Failed to get ziphash for %s@%s: %v", n.module, version, err)
			return nil, syscall.EIO
		}
		in := n.NewInode(ctx, &memFile{
			contents: []byte(zipHash),
			mode:     0644,
		}, fs.StableAttr{
			Mode: fuse.S_IFREG | 0644,
		})
		return in, 0
	}

	log.Printf("TODO: Lookup(%q) in module %q", name, n.module)
	return nil, syscall.EIO
}

var (
	verbose = flag.Bool("verbose", false, "enable verbose logging")
)

// This demonstrates how to build a file system in memory. The
// read/write logic for the file is provided by the MemRegularFile type.
func main() {
	flag.Parse()

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
		MountOptions: fuse.MountOptions{Debug: *verbose},
	})
	if err != nil {
		log.Panic(err)
	}

	log.Printf("Mounted on %s", mntDir)
	log.Printf("Unmount by calling 'umount' (macOS) or 'fusermount -u' (Linux) with arg %s", mntDir)

	server.Wait()
}
