package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/tailscale/gomodfs/stats"
	"github.com/tailscale/gomodfs/store"
	"github.com/tailscale/gomodfs/store/gitstore"
	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/sync/singleflight"
)

// FS is the gomodfs filesystem.
type FS struct {
	Git   *gitstore.Storage // legacy storage; TODO: remove this field, move all callers to Store interface
	Store store.Store

	Stats  *stats.Stats // or nil if stats are not enabled
	Client *http.Client // or nil to use default client

	sf singleflight.Group

	// ModuleProxyURL is the URL of the Go module proxy to use.
	// If empty, "https://proxy.golang.org" is used.
	// It should not have a trailing slash.
	ModuleProxyURL string
}

func (fs *FS) client() *http.Client {
	return cmp.Or(fs.Client, http.DefaultClient)
}

func (fs *FS) moduleProxyURL() string {
	if fs.ModuleProxyURL != "" {
		return strings.TrimSuffix(fs.ModuleProxyURL, "/")
	}
	return "https://proxy.golang.org"
}

func (fs *FS) modURLBase(mv store.ModuleVersion) (string, error) {
	escMod, err := module.EscapePath(mv.Module)
	if err != nil {
		return "", fmt.Errorf("failed to escape module name %q: %w", mv.Module, err)
	}
	escVer, err := module.EscapeVersion(mv.Version)
	if err != nil {
		return "", fmt.Errorf("failed to escape version %q: %w", mv.Version, err)
	}
	return fs.moduleProxyURL() + "/" + escMod + "/@v/" + escVer, nil
}

func (fs *FS) downloadModFile(ctx context.Context, mv store.ModuleVersion) (_ []byte, err error) {
	sp := fs.Stats.StartSpan("download-mod-file")
	defer func() { sp.End(err) }()

	ctx = context.Background() // TODO(bradfitz): make a singleflight variant that refcounts context lifetime

	vi, err, _ := fs.sf.Do("download-mod:"+mv.Module+"@"+mv.Version, func() (any, error) {
		urlBase, err := fs.modURLBase(mv)
		if err != nil {
			return nil, err
		}
		urlStr := urlBase + ".mod"

		data, err := fs.netSlurp(ctx, urlStr)
		if err != nil {
			return nil, fmt.Errorf("failed to download %q: %w", urlStr, err)
		}
		if err := fs.Store.PutModFile(ctx, mv, data); err != nil {
			return nil, fmt.Errorf("failed to store mod file for %q: %w", mv, err)
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return vi.([]byte), nil
}

func (fs *FS) downloadInfoFile(ctx context.Context, mv store.ModuleVersion) (_ []byte, err error) {
	sp := fs.Stats.StartSpan("download-info-file")
	defer func() { sp.End(err) }()

	ctx = context.Background() // TODO(bradfitz): make a singleflight variant that refcounts context lifetime

	vi, err, _ := fs.sf.Do("download-info:"+mv.Module+"@"+mv.Version, func() (any, error) {
		urlBase, err := fs.modURLBase(mv)
		if err != nil {
			return nil, err
		}
		urlStr := urlBase + ".info"

		data, err := fs.netSlurp(ctx, urlStr)
		if err != nil {
			return nil, fmt.Errorf("failed to download %q: %w", urlStr, err)
		}
		if err := fs.Store.PutInfoFile(ctx, mv, data); err != nil {
			return nil, fmt.Errorf("failed to store info file for %q: %w", mv, err)
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return vi.([]byte), nil
}

func (fs *FS) downloadZip(ctx context.Context, mv store.ModuleVersion) (store.ModHandle, error) {
	baseURL, err := fs.modURLBase(mv)
	if err != nil {
		return nil, err
	}

	download := map[string][]byte{} // extension (zip, info, mod) -> data
	for _, ext := range []string{"zip", "info", "mod"} {
		urlStr := baseURL + "." + ext
		sp := fs.Stats.StartSpan("net-downloadZip-ext-" + ext)
		data, err := fs.netSlurp(ctx, urlStr)
		sp.End(err)
		if err != nil {
			return nil, fmt.Errorf("failed to download %q: %w", urlStr, err)
		}
		download[ext] = data
		log.Printf("Downloaded %d bytes from %v", len(data), urlStr)
	}

	zr, err := zip.NewReader(bytes.NewReader(download["zip"]), int64(len(download["zip"])))
	if err != nil {
		return nil, fmt.Errorf("failed to unzip module data: %w", err)
	}

	put := store.PutModuleData{
		InfoFile: download["info"],
		ModFile:  download["mod"],
		Files:    make([]store.PutFile, 0, len(zr.File)),
	}

	fileNames := make([]string, 0, len(zr.File))
	zipOfFile := map[string]*zip.File{}
	for _, f := range zr.File {
		fileNames = append(fileNames, f.Name)
		zipOfFile[f.Name] = f
	}

	zipHash, err := dirhash.Hash1(fileNames, func(name string) (io.ReadCloser, error) {
		f := zipOfFile[name]
		if f == nil {
			return nil, fmt.Errorf("file %q not found in zip", name) // should never happen
		}
		return f.Open()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to hash zip contents: %w", err)
	}
	put.ZipHash = []byte(zipHash)

	for _, f := range zr.File {
		put.Files = append(put.Files, putFile{
			path: f.Name[len(mv.Module)+len("@")+len(mv.Version)+len("/"):], // remove module name and version prefix
			zf:   f,
		})
	}

	return fs.Store.PutModule(ctx, mv, put)
}

type putFile struct {
	path string
	zf   *zip.File
}

func (pf putFile) Path() string                 { return pf.path }
func (pf putFile) Size() int64                  { return int64(pf.zf.UncompressedSize64) }
func (pf putFile) Open() (io.ReadCloser, error) { return pf.zf.Open() }
func (pf putFile) Mode() os.FileMode            { return pf.zf.Mode() }

func (fs *FS) netSlurp(ctx0 context.Context, urlStr string) (ret []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx0, 30*time.Second)
	defer cancel()

	defer func() {
		if err != nil && ctx0.Err() == nil {
			log.Printf("netSlurp(%q) failed: %v", urlStr, err)
		}
		if err == nil {
			//log.Printf("netSlurp(%q) succeeded; %d bytes", urlStr, len(ret))
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %q: %w", urlStr, err)
	}
	res, err := fs.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download %q: %w", urlStr, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download %q: %s", urlStr, res.Status)
	}
	return io.ReadAll(res.Body)
}

func (fs *FS) getZipRoot(ctx context.Context, mv store.ModuleVersion) (mh store.ModHandle, err error) {
	span := fs.Stats.StartSpan("get-zip-root")
	defer func() { span.End(err) }()

	ctx = context.Background() // TODO(bradfitz): make a singleflight variant that refcounts context lifetime

	rooti, err, _ := fs.sf.Do("get-zip-root:"+mv.Module+"@"+mv.Version, func() (any, error) {
		root, err := fs.Store.GetZipRoot(ctx, mv)
		if err == nil {
			return root, nil
		}
		if !errors.Is(err, store.ErrCacheMiss) {
			return nil, fmt.Errorf("failed to get zip root for %v: %w", mv, err)
		}

		span := fs.Stats.StartSpan("get-zip-root-cache-fill")
		root, err = fs.downloadZip(ctx, mv)
		span.End(err)
		if err != nil {
			return nil, err
		}
		return root, nil
	})
	if err != nil {
		return nil, err
	}
	return rooti.(store.ModHandle), nil
}

func (fs *FS) getModFile(ctx context.Context, mv store.ModuleVersion) (data []byte, err error) {
	return fs.getMetaFile(ctx, mv, "mod", fs.Store.GetModFile, fs.downloadModFile)
}
func (fs *FS) getInfoFile(ctx context.Context, mv store.ModuleVersion) (data []byte, err error) {
	return fs.getMetaFile(ctx, mv, "info", fs.Store.GetInfoFile, fs.downloadInfoFile)
}

func (fs *FS) getMetaFile(ctx context.Context, mv store.ModuleVersion, ext string,
	getFromStore,
	downloadAndFill func(context.Context, store.ModuleVersion) ([]byte, error)) (data []byte, err error) {

	v, err := getFromStore(ctx, mv)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, store.ErrCacheMiss) {
		return nil, fmt.Errorf("failed to get %s file for %v: %w", ext, mv, err)
	}
	v, err = downloadAndFill(ctx, mv)
	if err != nil {
		log.Printf("failed to download %s for %v: %v", ext, mv, err)
		return nil, syscall.EIO
	}
	return v, nil
}

func (fs *FS) getZiphash(ctx context.Context, mv store.ModuleVersion) (data []byte, err error) {
	zr, err := fs.getZipRoot(ctx, mv)
	if err != nil {
		log.Printf("Failed to get zip root for %v: %v", mv, err)
		return nil, err
	}
	return fs.Store.GetZipHash(ctx, zr)
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

func setLongTTL(out *fuse.EntryOut) {
	out.AttrValid = 86400
	out.EntryValid = 86400
}

func (n *moduleNameNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	ctx = maybeIgnoreIgnoreContext(ctx)

	setLongTTL(out)
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
		finalFrag, escVer, _ := strings.Cut(name, "@")
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
		ver, err := module.UnescapeVersion(escVer)
		if err != nil {
			log.Printf("Failed to unescape version %q: %v", escVer, err)
			return nil, syscall.EIO
		}
		mv := store.ModuleVersion{
			Module:  modName,
			Version: ver,
		}
		root, err := n.fs.getZipRoot(ctx, mv)
		if err != nil {
			log.Printf("Failed to get ziproot handle for module %q: %v", modName, err)
			return nil, syscall.EIO
		}
		return n.NewInode(ctx, &pathUnderZipRoot{
			fs:   n.fs,
			mv:   mv,
			root: root,
			mode: os.ModeDir,
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

// pathUnderZipRoot is some file or directory under a module's zip root.
type pathUnderZipRoot struct {
	fs.Inode
	fs   *FS
	mv   store.ModuleVersion // module version for this directory
	root store.ModHandle
	mode os.FileMode // either fs.ModeDir or regular file mode bits, either 0644 or 0755
	size int64       // for regular files; -1 if unknown (will require extra fetches)
	path string      // empty for root, else "dir" or "dir/subdir"; no trailing slash

	mu              sync.Mutex
	haveFileContent bool // whether fileContent is valid
	fileContent     []byte
	ents            []*store.Dirent
	ent             map[string]*store.Dirent // non-nil once ents is initialized
}

var (
	_ = (fs.NodeLookuper)((*pathUnderZipRoot)(nil))
	_ = (fs.NodeReaddirer)((*pathUnderZipRoot)(nil))
	_ = (fs.NodeReader)((*pathUnderZipRoot)(nil))
	_ = (fs.NodeGetattrer)((*pathUnderZipRoot)(nil))
	_ = (fs.NodeOpener)((*pathUnderZipRoot)(nil))
)

func (n *pathUnderZipRoot) initDirEntsLocked(ctx context.Context) error {
	if n.ent != nil {
		return nil
	}
	span := n.fs.Stats.StartSpan("Store.Readdir")
	ents, err := n.fs.Store.Readdir(ctx, n.root, n.path)
	span.End(err)
	if err != nil {
		return fmt.Errorf("failed to get dir files for %q: %w", n.path, err)
	}
	n.ent = make(map[string]*store.Dirent)
	for _, e := range ents {
		n.ent[e.Name] = &e
		n.ents = append(n.ents, &e)
	}
	return nil
}

func (n *pathUnderZipRoot) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	defer n.fs.Stats.StartSpan("pathUnderZipRoot-Getattr").End(nil)
	out.AttrValid = 86400 * 90 // valid for 90 days
	out.Size = uint64(n.size)
	out.Mode = fuseMode(n.mode)
	return 0
}

func (n *pathUnderZipRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (_ *fs.Inode, errno syscall.Errno) {
	ctx = maybeIgnoreIgnoreContext(ctx)

	if !n.mode.IsDir() {
		log.Printf("Lookup called on non-directory %q", n.path)
		return nil, syscall.ENOTDIR
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if err := n.initDirEntsLocked(ctx); err != nil {
		log.Printf("Lookup(%v, %q, %q): %v", n.mv, n.path, name, err)
		return nil, syscall.EIO
	}
	setLongTTL(out)

	ent, ok := n.ent[name]
	if !ok {
		return nil, syscall.ENOENT
	}

	path := name
	if n.path != "" {
		path = n.path + "/" + name
	}
	out.Size = uint64(ent.Size)
	return n.NewInode(ctx, &pathUnderZipRoot{
		fs:   n.fs,
		mv:   n.mv,
		root: n.root,
		path: path,
		mode: ent.Mode,
		size: ent.Size,
	}, fs.StableAttr{Mode: fuseMode(ent.Mode)}), 0
}

// fuseMode maps the limited subset of os.FileMode values that
// Go module zip files use to FUSE file modes.
// It's also approximately what git uses.
// (Git can do symlinks, but Go module zip files don't.)
func fuseMode(m os.FileMode) uint32 {
	if m.IsDir() {
		return fuse.S_IFDIR | 0755
	}
	if m&0o111 != 0 {
		return fuse.S_IFREG | 0755 // executable file
	}
	return fuse.S_IFREG | 0644
}

func (n *pathUnderZipRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	ctx = maybeIgnoreIgnoreContext(ctx)

	n.mu.Lock()
	defer n.mu.Unlock()

	if err := n.initDirEntsLocked(ctx); err != nil {
		log.Printf("Readdir(%v, %q) initDirEnts error: %v", n.mv, n.path, err)
		return nil, syscall.EIO
	}

	ents := make([]fuse.DirEntry, len(n.ents))
	for i, ge := range n.ents {
		ents[i] = fuse.DirEntry{
			Name: ge.Name,
			Mode: fuseMode(ge.Mode),
			Off:  uint64(i),
		}
	}
	return &dirStream{ents: ents}, 0
}

func isReadonlyOpenFlags(flags uint32) bool {
	if flags == 0 {
		return true
	}
	if runtime.GOOS == "linux" && flags == 0x8000 {
		// Permit O_LARGEFILE on Linux.
		return true
	}
	return false
}

func (n *pathUnderZipRoot) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if n.mode.IsDir() {
		log.Printf("Open called on directory %q", n.path)
		return nil, 0, syscall.EISDIR
	}
	if !isReadonlyOpenFlags(flags) {
		log.Printf("non-readonly open(%v %q) with flags %x", n.mv, n.path, flags)
		return nil, 0, syscall.EINVAL
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

// useGoFUSECtx, if true, makes gomodfs ignore the contexts
// passed from go-fuse and instead just always uses context.Background().
var useGoFUSECtx = os.Getenv("USE_GO_FUSE_CONTEXT") == "1"

func maybeIgnoreIgnoreContext(ctx context.Context) context.Context {
	if useGoFUSECtx {
		return ctx
	}
	return context.Background()
}

func (n *pathUnderZipRoot) Read(ctx context.Context, h fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	ctx = maybeIgnoreIgnoreContext(ctx)

	if n.mode.IsDir() {
		log.Printf("Read called on directory %q", n.path)
		return nil, syscall.EISDIR
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	span := n.fs.Stats.StartSpan("pathUnderZipRoot-Read")
	defer span.End(nil) // TODO: track errors for stats?

	if !n.haveFileContent {
		var err error
		span := n.fs.Stats.StartSpan("GetFile")
		n.fileContent, err = n.fs.Store.GetFile(ctx, n.root, n.path)
		span.End(err)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("GetFile(%v, %q) failed: %v", n.mv, n.path, err)
			}
			return nil, syscall.EIO
		}
		n.haveFileContent = true
	}

	if off > math.MaxInt {
		// TODO: care about 32-bit machines? not today. but 2GB source files
		// seem unlikely.
		log.Printf("Read called with off %d, which is too large", off)
		return nil, syscall.EINVAL
	}

	end := int(off) + len(dest)
	if end < int(off) {
		log.Printf("Read called with off %d, which is too large", off)
		return nil, syscall.EINVAL
	}

	if end > len(n.fileContent) {
		end = len(n.fileContent)
	}
	return fuse.ReadResultData(n.fileContent[int(off):end]), 0
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

func (f *memFile) Read(_ context.Context, h fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	end := int(off) + len(dest)
	if end > len(f.contents) {
		end = len(f.contents)
	}
	return fuse.ReadResultData(f.contents[off:end]), 0
}

func (f *memFile) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if !isReadonlyOpenFlags(flags) {
		log.Printf("non-readonly open with flags %x", flags)
		return nil, 0, syscall.EINVAL

	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *memFile) Getattr(_ context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.AttrValid = 86400 * 90 // valid forever; 90 days will do
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

func (f *symLink) Getattr(_ context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.AttrValid = 86400 * 90 // valid forever; 90 days will do
	out.Mode = fuse.S_IFLNK | (f.mode & 0o777)
	return 0
}

func (f *symLink) Readlink(_ context.Context) ([]byte, syscall.Errno) {
	return f.contents, 0
}

type dirStream struct {
	ents []fuse.DirEntry
}

func (s *dirStream) HasNext() bool { return len(s.ents) > 0 }
func (s *dirStream) Close()        {}
func (s *dirStream) Next() (fuse.DirEntry, syscall.Errno) {
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
	ctx = maybeIgnoreIgnoreContext(ctx)

	setLongTTL(out)
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
	ctx = maybeIgnoreIgnoreContext(ctx)
	setLongTTL(out)

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
func (n *cacheDownloadNode) lookupUnderModule(ctx context.Context, name string, out *fuse.EntryOut) (_ *fs.Inode, retErrNo syscall.Errno) {
	ctx = maybeIgnoreIgnoreContext(ctx)

	// We are in the /@v/ directory, so we should return a file. e.g. one of these:
	// /tmp/dl/cache/download/github.com/!microsoft/go-winio/@v/v0.6.2.info
	// /tmp/dl/cache/download/github.com/!microsoft/go-winio/@v/v0.6.2.mod
	// /tmp/dl/cache/download/github.com/!microsoft/go-winio/@v/v0.6.2.ziphash

	if strings.HasSuffix(name, ".partial") {
		return nil, syscall.ENOENT
	}

	dotExt := filepath.Ext(name)
	switch dotExt {
	case ".info", ".mod", ".ziphash":
		ext := dotExt[1:] // "info", "mod", "ziphash"

		sp := n.fs.Stats.StartSpan("get-metafile-" + ext)
		defer func() {
			if retErrNo != 0 {
				sp.End(retErrNo)
			} else {
				sp.End(nil)
			}
		}()

		version := strings.TrimSuffix(name, dotExt) // "v0.0.0-20240501181205-ae6ca9944745"
		mv := store.ModuleVersion{
			Module:  n.module,
			Version: version,
		}

		var v []byte
		var err error
		switch ext {
		case "mod":
			v, err = n.fs.getModFile(ctx, mv)
		case "info":
			v, err = n.fs.getInfoFile(ctx, mv)
		case "ziphash":
			v, err = n.fs.getZiphash(ctx, mv)
		}
		if err != nil {
			log.Printf("Failed to get %s file for %v: %v", ext, mv, err)
			return nil, syscall.EIO
		}
		out.Size = uint64(len(v))
		in := n.NewInode(ctx, &memFile{
			contents: v,
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
	debugListen = flag.String("http-debug", "", "if set, listen on this address for a debug HTTP server")
	verbose     = flag.Bool("verbose", false, "enable verbose logging")
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

	mntDir := filepath.Join(os.Getenv("HOME"), "mnt-gomodfs")
	exec.Command("umount", mntDir).Run() // best effort
	if os.Getenv("GOOS") == "darwin" {
		exec.Command("diskutil", "unmount", "force", mntDir).Run() // best effort
	}

	if err := os.MkdirAll(mntDir, 0755); err != nil {
		log.Panicf("Failed to create mount directory %s: %v", mntDir, err)
	}

	st := &stats.Stats{}
	gitStore := &gitstore.Storage{
		GitRepo: gitCache,
		Stats:   st,
	}
	mfs := &FS{
		Git:   gitStore,
		Store: gitStore,
		Stats: st,
	}

	if *debugListen != "" {
		ln, err := net.Listen("tcp", *debugListen)
		if err != nil {
			log.Fatalf("Failed to listen on %s: %v", *debugListen, err)
		}
		log.Printf("Debug HTTP server listening on %s", *debugListen)
		hs := &http.Server{
			Handler: mfs,
		}
		go hs.Serve(ln)
	}

	root := &moduleNameNode{
		fs: mfs,
	}
	server, err := fs.Mount(mntDir, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug:         *verbose,
			FsName:        "gomodfs",
			DisableXAttrs: true,
		},
	})
	if err != nil {
		log.Panic(err)
	}

	log.Printf("Mounted on %s", mntDir)
	log.Printf("Unmount by calling 'umount' (macOS) or 'fusermount -u' (Linux) with arg %s", mntDir)

	server.Wait()
}

func (s *FS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Stats.ServeHTTP(w, r)
}
