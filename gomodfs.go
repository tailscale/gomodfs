package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tailscale/gomodfs/storage/gitstore"
	"go4.org/mem"
	"golang.org/x/mod/module"
)

// FS is the gomodfs filesystem.
type FS struct {
	Git   *gitstore.Storage // or nil to use default client
	Stats Stats
}

type OpStat struct {
	Count int
	Errs  int
	Total time.Duration
}

type Stats struct {
	mu  sync.Mutex
	ops map[string]*OpStat
}

type ActiveSpan struct {
	st    *Stats
	op    string
	start time.Time
	done  bool
}

func (s *Stats) StartSpan(op string) *ActiveSpan {
	return &ActiveSpan{
		st:    s,
		op:    op,
		start: time.Now(),
		done:  false,
	}
}

func (s *ActiveSpan) End(err error) {
	if s.done {
		panic("End called twice on span")
	}
	s.done = true

	st := s.st
	st.mu.Lock()
	defer st.mu.Unlock()

	op, ok := st.ops[s.op]
	if !ok {
		if st.ops == nil {
			st.ops = make(map[string]*OpStat)
		}
		op = &OpStat{}
		st.ops[s.op] = op
	}
	op.Count++
	if err != nil {
		op.Errs++
	}
	d := time.Since(s.start)
	op.Total += d

	log.Printf("Op %s took %v (calls %v; errs=%d; avg=%v)",
		s.op,
		d.Round(time.Millisecond), op.Count, op.Errs, (op.Total / time.Duration(op.Count)).Round(time.Millisecond))
}

type moduleNameNode struct {
	fs.Inode
	paths []string
	fs    *FS

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
	if len(n.paths) == 0 {
		if name == "cache" {
			return n.NewInode(ctx, &cacheRootNode{
				fs: n.fs,
			}, fs.StableAttr{
				Mode: fuse.S_IFDIR | 0755,
			}), 0
		}
		// As a special case for Tailscale's needs unrelated to the GOMODCACHE
		// layout (because I'm too lazy to write a separate FUSE filesystem
		// that's 95% identical and factor stuff out today), treat a directory
		// at the top level named "tsgo-$GOOS" as Tailscale's ~/.cache/tsgo/ directory
		// as expected by ./tool/go.
		if suf, ok := strings.CutPrefix(name, "tsgo-"); ok {
			goos, goarch, ok := strings.Cut(suf, "-")
			if !ok {
				log.Printf("Invalid tsgo name %q, expected tsgo-$GOOS-$GOARCH", name)
				return nil, syscall.ENOENT
			}
			switch goos {
			case "linux", "darwin", "windows":
			default:
				return nil, syscall.ENOENT
			}
			switch goarch {
			case "amd64", "arm64":
			default:
				return nil, syscall.ENOENT
			}
			return n.NewInode(ctx, &tsgoRoot{
				fs:     n.fs,
				goos:   goos,
				goarch: goarch,
			}, fs.StableAttr{
				Mode: fuse.S_IFDIR | 0755,
			}), 0
		}

	}
	if strings.Contains(name, "@") {
		finalFrag, ver, _ := strings.Cut(name, "@")
		var escName string
		if len(n.paths) > 0 {
			escName = strings.Join(n.paths, "/") + "/" + finalFrag
		} else {
			escName = finalFrag
		}
		modName, err := module.UnescapePath(escName)
		if err != nil {
			log.Printf("Failed to unescape module name %q: %v", escName, err)
			return nil, syscall.EIO
		}
		span := n.fs.Stats.StartSpan("module-name-get-repo")
		res, err := n.fs.Git.Get(ctx, modName+"@"+ver)
		span.End(err)
		if err != nil {
			log.Printf("Failed to get module %q: %v", modName, err)
			return nil, syscall.EIO
		}
		zipSpan := n.fs.Stats.StartSpan("getZipRoot")
		treeHash, err := n.fs.Git.GetZipRootTree(res.ModTree)
		zipSpan.End(err)
		if err != nil {
			log.Printf("Failed to get zip root tree for %q (%s): %v", modName, res.ModTree, err)
			return nil, syscall.EIO
		}
		log.Printf("lookup tree: mod=%s ver=%v tree=%v", modName, ver, treeHash)
		return n.NewInode(ctx, &treeNode{
			fs:   n.fs,
			tree: treeHash,
		}, fs.StableAttr{
			Mode: fuse.S_IFDIR | 0755,
		}), 0
	}
	in := n.Inode.NewInode(ctx, &moduleNameNode{
		fs:    n.fs,
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
	fs   *FS
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

var _ fs.NodeLookuper = (*treeNode)(nil)
var _ fs.NodeReaddirer = (*treeNode)(nil)

func (n *treeNode) initEnts() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.treeEnt != nil {
		return nil
	}

	span := n.fs.Stats.StartSpan("ls-tree")
	cmd := exec.Command("git", "-C", n.fs.Git.GitRepo, "ls-tree", n.tree)
	outData, err := cmd.CombinedOutput()
	span.End(err)
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
	tab := mem.S("\t")
	for bs.Scan() {
		modeTypeHash, name, ok := mem.Cut(mem.B(bs.Bytes()), tab)
		if !ok {
			return fmt.Errorf("unexpected ls-tree output: %q", bs.Text())
		}
		f = mem.AppendFields(f[:0], modeTypeHash)
		if len(f) != 3 {
			return fmt.Errorf("unexpected ls-tree output: %q", bs.Text())
		}
		mode, _ := mem.ParseUint(f[0], 8, 32)
		ents = append(ents, &gitTreeEnt{
			mode:    uint32(mode),
			gitType: gitType(f[1]),
			ref:     f[2].StringCopy(),
			name:    name.StringCopy(),
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
		log.Printf("Lookup(%q) initEnts error in tree %s: %v", name, n.tree, err)
		return nil, syscall.EIO
	}
	ge, ok := n.treeEnt[name]
	if !ok {
		return nil, syscall.ENOENT
	}
	switch ge.gitType {
	case "blob":
		span := n.fs.Stats.StartSpan("blob-cat-file")
		cmd := exec.Command("git", "-C", n.fs.Git.GitRepo, "cat-file", "-p", ge.ref)
		outData, err := cmd.CombinedOutput()
		span.End(err)
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
			fs:   n.fs,
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
	fs *FS
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
		fs:   n.fs,
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
	fs *FS

	segs   []string // path segments, e.g. ["!microsoft", "go-winio"] (empty if module is set)
	module string   // if non-empty, the module name unescaped, e.g. "Microsoft.com/go-winio" (in the /@v/ directory)
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
			fs:     n.fs,
			module: unescaped,
		}, fs.StableAttr{
			Mode: fuse.S_IFDIR | 0755,
		})
		return in, 0
	}

	in := n.NewInode(ctx, &cacheDownloadNode{
		fs:   n.fs,
		segs: append(n.segs[:len(n.segs):len(n.segs)], name),
	}, fs.StableAttr{
		Mode: fuse.S_IFDIR | 0755,
	})
	return in, 0
}

var infoTmpRx = regexp.MustCompile(`\.info\d+\.tmp$`)

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

	escPath, err := module.EscapePath(n.module)
	if err != nil {
		log.Printf("Failed to escape module name %q: %v", n.module, err)
		return nil, syscall.EIO
	}

	dotExt := filepath.Ext(name)
	switch dotExt {
	case ".info", ".mod", ".ziphash":
		ext := dotExt[1:] // "info", "mod", "ziphash"
		d := n.fs.Git
		if d == nil {
			log.Printf("No git repo configured, cannot resolve %s for %q", ext, n.module)
			return nil, syscall.EIO
		}
		version := strings.TrimSuffix(name, dotExt) // "v0.0.0-20240501181205-ae6ca9944745"

		// In case the module isn't downloaded yet, Get it for the side
		// effect of downloading it and populating all the refs.
		getRes, getErr := n.fs.Git.Get(ctx, n.module+"@"+version)

		span := n.fs.Stats.StartSpan("getMetaFile-" + ext)
		metaFile, err := d.GetMetaFile(escPath, version, ext)
		span.End(err)
		if err != nil {
			log.Printf("Failed to get %s for %s@%s: %v", ext, n.module, version, err)
			if getErr != nil {
				log.Printf("Also failed to earlier download %s@%s: %v", n.module, version, getErr)
			} else {
				log.Printf("Module %s@%s is %+v", n.module, version, getRes)
			}
			return nil, syscall.EIO
		}

		log.Printf("lookup meta: mod=%s file=%s", n.module, name)
		in := n.NewInode(ctx, &memFile{
			contents: []byte(metaFile),
			mode:     0644,
		}, fs.StableAttr{
			Mode: fuse.S_IFREG | 0644,
		})
		return in, 0
	}

	if infoTmpRx.MatchString(name) {
		// Ignore .infoNNN.tmp files. This is cmd/go trying to remove a field
		// from an info file on disk even though the field doesn't exist; it's
		// just getting confused because the serialized JSON doesn't match
		// byte-for-byte because some fields were reordered at some point and
		// modules older than a certain date have *.info files cached on
		// proxy.golang.org in different orders and this confused Go's best
		// effort field cleaning code. But it's fine if the write fails (e.g. on
		// a read-only filesystem) so we just correctly say it doesn't exist
		// here and deny the create later by not implementing Create, so the
		// default is EROFS (read-only filesystem).
		return nil, syscall.ENOENT
	}

	log.Printf("TODO: unhandled lookup(%q) in module %q", name, n.module)
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

	d := &gitstore.Storage{GitRepo: gitCache}

	mntDir := filepath.Join(os.Getenv("HOME"), "mnt-gomodfs")
	exec.Command("umount", mntDir).Run() // best effort

	if err := os.MkdirAll(mntDir, 0755); err != nil {
		log.Panicf("Failed to create mount directory %s: %v", mntDir, err)
	}

	mfs := &FS{
		Git: d,
	}

	root := &moduleNameNode{
		fs: mfs,
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
