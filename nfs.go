package gomodfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/tailscale/gomodfs/store"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
)

func (fs *FS) NFSHandler() nfs.Handler {
	// Create a new NFS handler that uses the gomodfs file system.
	return &NFSHandler{fs: fs}
}

// handle is the NFSv3 handle godmodfs uses. It's 64 bytes, so it's necessarily
// an NFSv3-only handle (too large for NFSv2).
//
// It's one of several forms:
//
//   - for the root, the handle is the zero value.
//   - when the module version is known, the first 32 bytes are the
//     SHA256(moduleName || moduleVersion) and the second 32 bytes
//     are the SHA256(path-in-zip) or SHA256("^" || cacheDownloadFileExt).
//   - when the module version is not known, the first 32 bytes are
//     the SHA256(path-from-root).
type handle [64]byte

// zeroHandle is the root handle.
var zeroHandle handle

type NFSHandler struct {
	fs *FS

	nfs.Handler // temporary embedding during dev to watch what panics

	mu        sync.Mutex
	handle    map[handle][]string // => path segments
	readCache map[handle]*readCacheEntry
}

type readCacheEntry struct {
	contents []byte
	path     string
	attr     *nfs.FileAttribute
	timer    *time.Timer // for the AfterFunc to remove it from the cache
}

var (
	_ nfs.Handler     = (*NFSHandler)(nil)
	_ nfs.ReadHandler = (*NFSHandler)(nil)
)

type billyFS struct {
	fs *FS
}

var (
	_ billy.Filesystem = billyFS{}
	_ billy.Capable    = billyFS{}
)

var errReadonly = errors.New("gomodfs is read-only")

func (billyFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}

func (b billyFS) Join(elem ...string) string { return path.Join(elem...) }

func (b billyFS) Open(filename string) (billy.File, error) { panic("unreachable") }
func (b billyFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	panic("unreachable")
}

func (b billyFS) Stat(filename string) (os.FileInfo, error) {
	panic(fmt.Sprintf("TODO billy Stat(%q)", filename))
}

func (b billyFS) Create(filename string) (billy.File, error)       { return nil, errReadonly }
func (b billyFS) Rename(oldpath, newpath string) error             { return errReadonly }
func (b billyFS) Remove(filename string) error                     { return errReadonly }
func (b billyFS) TempFile(dir, prefix string) (billy.File, error)  { return nil, errReadonly }
func (b billyFS) MkdirAll(filename string, perm os.FileMode) error { return errReadonly }
func (b billyFS) Symlink(target, link string) error                { return errReadonly }

func (b billyFS) Chroot(path string) (billy.Filesystem, error) {
	panic(fmt.Sprintf("TODO billy Chroot(%q)", path))
}

func (b billyFS) Root() string {
	log.Printf("NFS: billyFS.Root called")
	return "/"
}

func (b billyFS) ReadDir(path string) ([]os.FileInfo, error) {
	log.Printf("NFS ReadDir(%q)", path)
	switch path {
	case "":
		return []os.FileInfo{
			regFileInfo{
				name: statusFile,
				size: 100,
				mode: 0444,
			},
			dirFileInfo{baseName: "cache"},
		}, nil
	case "cache":
		return []os.FileInfo{
			dirFileInfo{baseName: "download"},
		}, nil
	}

	mp := parsePath(path)
	if mp.NotExist {
		return nil, os.ErrNotExist
	}
	if !mp.InZip {
		return nil, nil
	}

	ctx := context.TODO()
	mh, err := b.fs.getZipRoot(ctx, mp.ModVersion)
	if err != nil {
		return nil, err
	}
	spanRD := b.fs.Stats.StartSpan("nfs.Readdir")
	ents, err := b.fs.Store.Readdir(ctx, mh, mp.Path)
	spanRD.End(err)

	ret := make([]os.FileInfo, len(ents))
	for i, ent := range ents {
		if ent.Mode.IsDir() {
			ret[i] = dirFileInfo{baseName: ent.Name}
			continue
		}
		ret[i] = regFileInfo{
			name: ent.Name,
			size: ent.Size,
			mode: ent.Mode.Perm(),
		}
		// TODO: symlinks? but go modules and tsgo don't use them?
	}

	return ret, nil
}

func (b billyFS) Lstat(filename string) (os.FileInfo, error) {
	mp := parsePath(filename)
	if mp.NotExist {
		return nil, os.ErrNotExist
	}
	switch mp.WellKnown {
	case "":
		// Nothing
	case statusFile:
		j := b.fs.statusJSON()
		return regFileInfo{
			name:       statusFile,
			size:       int64(len(j)),
			mode:       0444,
			modTimeNow: true,
		}, nil
	case "tsgo.extracted":
		return regFileInfo{
			name:       "tsgo.extracted",
			size:       0, // empty file
			mode:       0444,
			modTimeNow: true,
		}, nil
	default:
		return nil, os.ErrNotExist
	}

	ctx := context.TODO()

	if ext := mp.CacheDownloadFileExt; ext != "" {
		v, err := b.fs.getMetaFileByExt(ctx, mp.ModVersion, ext)
		if err != nil {
			log.Printf("Failed to get %s file for %v: %v", ext, mp.ModVersion, err)
			return nil, syscall.EIO
		}
		return regFileInfo{name: filepath.Base(filename), size: int64(len(v))}, nil
	}

	if !mp.InZip {
		// Guess we're still building a directory.
		return dirFileInfo{}, nil
	}

	mh, err := b.fs.getZipRoot(ctx, mp.ModVersion)
	if err != nil {
		return nil, err
	}

	spSS := b.fs.Stats.StartSpan("nfs.Lstat/Store.Stat")
	fi, err := b.fs.Store.Stat(ctx, mh, mp.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			spSS.End(nil)
			return nil, os.ErrNotExist
		}
		log.Printf("Failed to get file %q in zip for %v: %v", mp.Path, mp.ModVersion, err)
		spSS.End(err)
		return nil, err
	}
	spSS.End(nil)
	return fi, nil
}

func (b billyFS) Readlink(link string) (string, error) {
	panic(fmt.Sprintf("TODO billy Readlink(%q)", link))
}

func (h *NFSHandler) Mount(ctx context.Context, c net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	log.Printf("NFS mount request from %s for %+v, %q", c.RemoteAddr(), req.Header, req.Dirpath)
	return nfs.MountStatusOk, billyFS{fs: h.fs}, []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

func (h *NFSHandler) Change(billy.Filesystem) billy.Change {
	return nil // read-only filesystem
}

func (h *NFSHandler) FSStat(ctx context.Context, fs billy.Filesystem, stat *nfs.FSStat) error {
	stat.TotalFiles = 123 // TODO: populate all these with things like number of modules cached?
	return nil
}

func (h *NFSHandler) ToHandle(fs billy.Filesystem, path []string) []byte {
	if len(path) == 0 {
		return zeroHandle[:]
	}

	pathStr := strings.Join(path, "/")
	pp := parsePath(pathStr)
	ret := make([]byte, sha256.Size*2) // 64 bytes; max handle length w/ NFS v3

	if pp.ModVersion.Module != "" {
		s := sha256.New()
		io.WriteString(s, pp.ModVersion.Module)
		io.WriteString(s, pp.ModVersion.Version)
		s.Sum(ret[:0])
	}
	if pp.CacheDownloadFileExt != "" {
		_ = append(ret[sha256.Size:][:0], pp.CacheDownloadFileExt...)
	} else {
		s := sha256.New()
		if pp.InZip {
			io.WriteString(s, pp.Path)
		} else {
			io.WriteString(s, pathStr)
		}
		s.Sum(ret[sha256.Size:][:0])
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handle == nil {
		h.handle = make(map[handle][]string)
	}
	handle := handle(ret)
	if _, ok := h.handle[handle]; !ok {
		h.handle[handle] = slices.Clone(path)
	}
	return ret
}

func (h *NFSHandler) FromHandle(handleb []byte) (billy.Filesystem, []string, error) {
	if len(handleb) != 64 {
		log.Printf("non-64-length handle %q", handleb)
		return nil, nil, errors.New("invalid handle")
	}
	handle := handle(handleb)

	if handle == zeroHandle {
		return billyFS{fs: h.fs}, nil, nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	pathSeg, ok := h.handle[handle]
	if !ok {
		log.Printf("TODO: NFS FromHandle called with unknown handle %q", handle)
		return nil, nil, errors.New("unknown handle")
	}
	return billyFS{fs: h.fs}, slices.Clone(pathSeg), nil
}

func (h *NFSHandler) InvalidateHandle(fs billy.Filesystem, fh []byte) error {
	log.Printf("NFS InvalidateHandle called with fs=%v, fh=%q", fs, fh)
	return nil
}

func (h *NFSHandler) HandleLimit() int {
	return math.MaxInt
}

func (n *NFSHandler) OnNFSRead(ctx context.Context, handleb []byte, offset uint64, count uint32) (*nfs.NFSReadResult, error) {
	if len(handleb) != 64 {
		log.Printf("non-64-length handle %q", handleb)
		return nil, &nfs.NFSStatusError{
			NFSStatus:  nfs.NFSStatusStale,
			WrappedErr: errors.New("wrong length handle"),
		}
	}

	handle := handle(handleb)
	end := offset + uint64(count)

	contents, attr, err := n.getFileContents(ctx, handle, end)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &nfs.NFSStatusError{
				NFSStatus:  nfs.NFSStatusNoEnt,
				WrappedErr: err,
			}
		}
		return nil, err
	}

	res := &nfs.NFSReadResult{
		Attr: attr,
	}
	data := contents[min(uint64(len(contents)), offset):]
	count = min(count, 16<<20)
	if len(data) > int(count) {
		data = data[:count]
	}
	res.Data = data
	if int(offset)+len(data) == len(contents) {
		res.EOF = true
	}
	return res, nil
}

func (n *NFSHandler) getFileContents(ctx context.Context, h handle, readEnd uint64) ([]byte, *nfs.FileAttribute, error) {
	n.mu.Lock()
	ent, ok := n.readCache[h]
	if ok {
		defer n.mu.Unlock()
		ent.timer.Reset(10 * time.Second)
		return ent.contents, ent.attr, nil
	}

	pathSeg, ok := n.handle[h]
	n.mu.Unlock()
	if !ok {
		log.Printf("TODO: NFS FromHandle called with unknown handle %q", h)
		return nil, nil, &nfs.NFSStatusError{
			NFSStatus:  nfs.NFSStatusStale,
			WrappedErr: errors.New("unknown handle"),
		}
	}

	filename := strings.Join(pathSeg, "/")
	contents, attr, err := n.getFileContentsUncached(ctx, filename)
	if err != nil {
		return nil, nil, err
	}

	if uint64(len(contents)) <= readEnd {
		// The caller probably won't be coming back for more reads.
		// No need to cache it.
		return contents, attr, nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	ent = &readCacheEntry{
		path:     filename,
		contents: contents,
		attr:     attr,
		timer: time.AfterFunc(10*time.Second, func() {
			n.removeFileCache(h)
		}),
	}
	if n.readCache == nil {
		n.readCache = map[handle]*readCacheEntry{}
	}
	n.readCache[h] = ent
	return contents, attr, err
}

func (n *NFSHandler) removeFileCache(h handle) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ent, ok := n.readCache[h]
	if !ok {
		return
	}
	log.Printf("NFS: dropping read cache entry for %q", ent.path)
	delete(n.readCache, h)
}

func (n *NFSHandler) getFileContentsUncached(ctx context.Context, filename string) (data []byte, attr *nfs.FileAttribute, err error) {
	attr = &nfs.FileAttribute{
		Type:     nfs.FileTypeRegular,
		FileMode: 0444, // unless overridden later
		Nlink:    1,
	}
	hasher := fnv.New64()
	_, _ = hasher.Write([]byte(filename))
	attr.Fileid = hasher.Sum64()
	attr.Atime = nfs.ToNFSTime(store.FakeStaticFileTime)
	attr.Mtime = attr.Atime
	attr.Ctime = attr.Atime

	defer func() {
		if err == nil {
			attr.Filesize = uint64(len(data))
			attr.Used = uint64(len(data))
		}
	}()

	mp := parsePath(filename)
	if mp.NotExist {
		return nil, nil, os.ErrNotExist
	}
	switch mp.WellKnown {
	case "":
		// nothing
	case "tsgo.extracted":
		return nil, attr, nil
	case statusFile:
		j := n.fs.statusJSON()
		return j, attr, nil
	default:
		return nil, nil, os.ErrNotExist
	}

	if ext := mp.CacheDownloadFileExt; ext != "" {
		sp := n.fs.Stats.StartSpan("nfs.OpenFile-ext-" + ext)
		v, err := n.fs.getMetaFileByExt(ctx, mp.ModVersion, ext)
		sp.End(err)
		if err != nil {
			log.Printf("Failed to get %s file for %v: %v", mp.CacheDownloadFileExt, mp.ModVersion, err)
			return nil, nil, err
		}
		return v, attr, nil
	}

	if !mp.InZip {
		n.fs.Stats.StartSpan("nfs.Open-not-zip").End(nil)
		return nil, nil, errors.New("open of non-file")
	}

	mh, err := n.fs.getZipRoot(ctx, mp.ModVersion)
	if err != nil {
		return nil, nil, err
	}
	spanGF := n.fs.Stats.StartSpan("nfs.getFileContents-StatGetFile")

	fi, err := n.fs.Store.Stat(ctx, mh, mp.Path)
	if err != nil {
		spanGF.End(err)
		return nil, nil, err
	}
	if fi.IsDir() {
		err = fmt.Errorf("gomodfs OpenFile %q is a directory, not a file", filename)
		spanGF.End(err)
		return nil, nil, err
	}
	attr.FileMode = uint32(fi.Mode().Perm())

	contents, err := n.fs.Store.GetFile(ctx, mh, mp.Path)
	if err != nil {
		spanGF.End(err)
		return nil, nil, err
	}
	spanGF.End(nil)
	return contents, attr, nil

}
