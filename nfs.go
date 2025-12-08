// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/tailscale/gomodfs/store"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
	"tailscale.com/util/mak"
)

func (fs *FS) NFSHandler() nfs.Handler {
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

var statusHandle = handle{0: 'S', 1: 'T', 2: 'A', 3: 'T', 4: 'U', 5: 'S'}

type NFSHandler struct {
	fs          *FS
	nfs.Handler // temporary embedding during dev to watch what panics
}

type handleTarget struct {
	path string   // the full path from the root, slash separated
	segs []string // strings.Split(path, "/") form
}

// statusMeta is a snapshot of the [statusFile] contents:
// both its FileInfo (notably its ModTime and Size) and its matching
// JSON.
type statusMeta struct {
	fi   os.FileInfo // with ModTime set to when it was generated
	json []byte
}

// statusFile generates or returns a recent snapshot of the status file.
func (h *NFSHandler) statusFile() *statusMeta {
	fs := h.fs
	fs.mu.Lock()
	defer fs.mu.Unlock()
	const regenEvery = 2 * time.Second
	if fs.statusCache != nil && time.Since(fs.statusCache.fi.ModTime()) < regenEvery {
		return fs.statusCache
	}
	j := h.fs.statusJSON()
	fs.statusCache = &statusMeta{
		fi: regFileInfo{
			name:    statusFile,
			size:    int64(len(j)),
			mode:    0444,
			modTime: time.Now(),
		},
		json: j,
	}
	return fs.statusCache
}

type readCacheEntry struct {
	attr     *nfs.FileAttribute
	blobHash blobHash
}

type blobHash [sha256.Size]byte

var (
	_ nfs.Handler     = (*NFSHandler)(nil)
	_ nfs.ReadHandler = (*NFSHandler)(nil)
)

type billyFS struct {
	fs *FS
	h  *NFSHandler
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
	if b.fs.Verbose {
		b.fs.logf("NFS: billyFS.Root called")
	}
	return "/"
}

func (b billyFS) ReadDir(path string) ([]os.FileInfo, error) {
	if b.fs.Verbose {
		b.fs.logf("NFS ReadDir(%q)", path)
	}
	switch path {
	case "":
		m := b.h.statusFile()
		return []os.FileInfo{
			m.fi,
			dirFileInfo{baseName: "cache"},
			openSetFile,
		}, nil
	case "cache":
		return []os.FileInfo{
			dirFileInfo{baseName: "download"},
			openSetFile,
		}, nil
	}

	mp := parsePath(path)
	if mp.NotExist {
		return nil, os.ErrNotExist
	}
	if !mp.InZip {
		return []os.FileInfo{
			openSetFile,
		}, nil
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
	// TODO(bradfitz): if the NFS layer passed in the NFS handle here (like if
	// we bypassed BillyFS and added a lower level NFS path like we did for
	// OnNFSRead), then we could check the NFS handle read cache and get its
	// attributes directy, without the hop through parsing the path and looking
	// up the ModHandle and then checking the git storage maps.

	mp := parsePath(filename)
	if mp.NotExist {
		return nil, os.ErrNotExist
	}
	switch mp.WellKnown {
	case "":
		// Nothing
	case statusFile:
		m := b.h.statusFile()
		return m.fi, nil
	case wkTSGoExtracted:
		return regFileInfo{
			name:       "TODO-tsgo-hash.extracted", // TODO(bradfitz): does it matter?
			size:       0,                          // empty file
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
			b.fs.logf("Failed to get %s file for %v: %v", ext, mp.ModVersion, err)
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
		b.fs.logf("Failed to get file %q in zip for %v: %v", mp.Path, mp.ModVersion, err)
		spSS.End(err)
		return nil, err
	}
	spSS.End(nil)
	return fi, nil
}

func (b billyFS) Readlink(link string) (string, error) {
	panic("unused")
}

func (h *NFSHandler) Mount(ctx context.Context, c net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	h.fs.logf("NFS mount request from %s for %+v, %q", c.RemoteAddr(), req.Header, req.Dirpath)
	return nfs.MountStatusOk, h.billyFS(), []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

func (h *NFSHandler) Change(billy.Filesystem) billy.Change {
	return nil // read-only filesystem
}

func (h *NFSHandler) FSStat(ctx context.Context, fs billy.Filesystem, stat *nfs.FSStat) error {
	stat.TotalFiles = 123 // TODO: populate all these with things like number of modules cached?
	return nil
}

var (
	notExistHandle = []byte("\x00\xFF\x00Nope")
	notExistSeg    = []string{".not-exist-path"}
)

const handleDebug = false

func (h *NFSHandler) ToHandle(_ billy.Filesystem, path []string) []byte {
	if len(path) == 0 {
		return zeroHandle[:]
	}
	if len(path) == 1 && path[0] == statusFile {
		return statusHandle[:]
	}

	pathStr := strings.Join(path, "/")
	pp := parsePath(pathStr)

	if pp.NotExist {
		return notExistHandle
	}

	ret := make([]byte, sha256.Size*2) // 64 bytes; max handle length w/ NFS v3

	if pp.ModVersion.Module != "" {
		hash := hashModVersion(pp.ModVersion)
		copy(ret[:sha256.Size], hash[:])
	}

	if pp.WellKnown == wkTSGoExtracted {
		hash := mkWellKnownPathHash(pp.WellKnown)
		copy(ret[sha256.Size:], hash[:])
	} else if pp.CacheDownloadFileExt != "" {
		ph := mkWellKnownPathHash(pp.CacheDownloadFileExt)
		copy(ret[sha256.Size:], ph[:])
	} else {
		s := sha256.New()
		if pp.InZip {
			io.WriteString(s, pp.Path)
		} else {
			io.WriteString(s, pathStr)
		}
		s.Sum(ret[sha256.Size:][:0])
	}

	fs := h.fs
	fs.mu.Lock()
	defer fs.mu.Unlock()
	handle := handle(ret)
	if _, ok := fs.handle[handle]; !ok {
		if handleDebug {
			h.fs.logf("HANDLE: ToHandle(%q) => %02x", pathStr, handle)
		}
		mak.Set(&fs.handle, handle, handleTarget{
			path: pathStr,
			segs: slices.Clone(path),
		})
	}
	return ret
}

func (h *NFSHandler) billyFS() billyFS {
	return billyFS{fs: h.fs, h: h}
}

func (h *NFSHandler) FromHandle(handleb []byte) (_ billy.Filesystem, segs []string, err error) {
	sp := h.fs.Stats.StartSpan("nfs.FromHandle")
	defer func() { defer sp.End(err) }()

	if bytes.Equal(handleb, notExistHandle) {
		return h.billyFS(), notExistSeg, nil
	}

	if len(handleb) != 64 {
		h.fs.logf("non-64-length handle %q", handleb)
		return nil, nil, errors.New("invalid handle")
	}

	ht, err := h.fromHandle(handle(handleb))
	if err != nil {
		if handleDebug {
			h.fs.logf("HANDLE: FromHandle(%02x) error: %v", handleb, err)
		}
		return nil, nil, err
	}
	if handleDebug {
		h.fs.logf("HANDLE: FromHandle(%02x) == %q", handleb, ht)
	}
	return h.billyFS(), ht.segs, nil
}

func staleErr(err error) error {
	return &nfs.NFSStatusError{
		NFSStatus:  nfs.NFSStatusStale,
		WrappedErr: err,
	}
}

// should return [staleErr] on a non-I/O-error-related miss.
func (h *NFSHandler) fromHandle(handle handle) (ret handleTarget, err error) {
	var zero handleTarget

	if handle == zeroHandle {
		return zero, nil // zero target is the root
	}
	if handle == statusHandle {
		return mkTargetFromPath(statusFile), nil
	}

	fs := h.fs
	fs.mu.Lock()
	targ, ok := fs.handle[handle]
	fs.mu.Unlock()
	if ok {
		return targ, nil
	}

	// Populate handle cache for the future on any success below.
	defer func() {
		if err == nil {
			if ret.path == "" || ret.segs == nil {
				panic("shouldn't happen")
			}
			fs.mu.Lock()
			mak.Set(&fs.handle, handle, ret)
			fs.mu.Unlock()
		}
	}()

	modVerHash := modVerHash(handle[:sha256.Size]) // first 32 bytes are sha256(module version)
	wantFileHash := pathHash(handle[sha256.Size:]) // second 32 bytes are sha256(file path)

	if modVerHash.IsZero() {
		return h.fs.handleTargetWithPathHash(wantFileHash)
	}

	// If we have a module version hash, we need to find the corresponding module version.
	mv, err := h.fs.moduleVersionWithHash(modVerHash)
	if err != nil {
		return zero, err
	}
	if !mv.IsValid() {
		h.fs.logf("miss mapping NFS handle %02x back to a module version", handle)
		return zero, staleErr(fmt.Errorf("unknown module version from NFS handle %02x", handle))
	}

	switch wantFileHash {
	case cdFileInfo:
		return mkTargetFromPath(cdPath(mv, "info")), nil
	case cdFileMod:
		return mkTargetFromPath(cdPath(mv, "mod")), nil
	case cdFileZiphash:
		return mkTargetFromPath(cdPath(mv, "ziphash")), nil
	case pathHashTSGo:
		if trip, ok := isTSGoModule(mv); ok {
			return mkTargetFromPath(tsGoZipRoot(trip) + ".extracted"), nil
		}
	}

	ctx := context.TODO()
	mh, err := h.fs.Store.GetZipRoot(ctx, mv)
	if err != nil {
		h.fs.logf("GetZipRoot error for %v: %v", mv, err)
		return zero, errors.New("unknown module version in handle")
	}

	for pathRes := range h.fs.walkStoreModulePaths(ctx, mh) {
		path, err := pathRes.Value()
		if err != nil {
			h.fs.logf("error getting path from walkStoreModulePaths: %v", err)
			break
		}
		hash := mkPathHash(path)
		if hash == wantFileHash {
			prefix, err := modVerZipPrefix(mv)
			if err != nil {
				h.fs.logf("error getting zip prefix for %v: %v", mv, err)
				break
			}
			return mkTargetFromPath(emptyPathJoin(prefix, path)), nil
		}
	}

	err = fmt.Errorf("didn't find matching file for NFS handle in %v", mv)
	h.fs.logf("miss inside zip root %v in FromHandle: %v", mv, err)
	return zero, staleErr(err)
}

func mkTargetFromPath(path string) handleTarget {
	return handleTarget{path: path, segs: splitSegs(path)}
}

func mkPathHash(path string) pathHash {
	var hash [sha256.Size]byte
	s2 := sha256.New()
	io.WriteString(s2, path)
	s2.Sum(hash[:0])
	return hash
}

func emptyPathJoin(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" {
		return b
	}
	return a + "/" + b
}

func (h *NFSHandler) InvalidateHandle(fs billy.Filesystem, fh []byte) error {
	h.fs.logf("NFS InvalidateHandle called with fs=%v, fh=%q", fs, fh)
	return nil
}

func (h *NFSHandler) HandleLimit() int {
	return math.MaxInt
}

func (n *NFSHandler) OnNFSRead(ctx context.Context, handleb []byte, offset uint64, count uint32) (*nfs.NFSReadResult, error) {
	if len(handleb) != 64 {
		n.fs.logf("non-64-length handle %q", handleb)
		return nil, &nfs.NFSStatusError{
			NFSStatus:  nfs.NFSStatusStale,
			WrappedErr: errors.New("wrong length handle"),
		}
	}

	handle := handle(handleb)

	contents, attr, err := n.getFileContents(ctx, handle)
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

func (n *NFSHandler) getFileContents(ctx context.Context, h handle) ([]byte, *nfs.FileAttribute, error) {
	fs := n.fs
	fs.mu.Lock()
	ent, ok := fs.entCache.GetOk(h)
	if ok {
		blob, ok := fs.blobCache.GetOk(ent.blobHash)
		if ok {
			fs.mu.Unlock()
			fs.MetricFileContentCacheHit.Add(1)
			return blob, ent.attr, nil
		}
	}
	fs.mu.Unlock()

	fs.MetricFileContentCacheMiss.Add(1)

	ht, err := n.fromHandle(h)
	if err != nil {
		n.fs.logf("TODO: NFS FromHandle error for %02x: %v", h, err)
		return nil, nil, &nfs.NFSStatusError{
			NFSStatus:  nfs.NFSStatusStale,
			WrappedErr: errors.New("unknown handle"),
		}
	}
	filename := ht.path

	contents, attr, err := n.getFileContentsUncached(ctx, filename)
	if err != nil {
		return nil, nil, err
	}

	fs.MetricFileContentCacheFill.Add(1)

	fs.setReadCache(h, attr, contents)
	return contents, attr, err
}

func (fs *FS) decrBlobCountLocked(h blobHash) {
	v := fs.blobCount[h]
	if v <= 0 {
		panic("bogus decrement")
	}
	if v == 1 {
		delete(fs.blobCount, h)
		fs.blobCache.Delete(h)
	} else {
		fs.blobCount[h] = v - 1
	}
}

func (fs *FS) setReadCache(h handle, attr *nfs.FileAttribute, contents []byte) {
	ent := &readCacheEntry{
		attr:     attr,
		blobHash: sha256.Sum256(contents),
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Initialize fs's blob-related caches on the first blob put.
	// (The FS type doesn't have a constructor that does this.)
	if fs.blobCount == nil {
		fs.blobCount = make(map[blobHash]int)
		fs.blobCache.EntrySize = func(key blobHash, value []byte) int64 {
			return int64(len(value))
		}
	}

	defer func() {
		fs.MetricFileEntryCount.Set(int64(fs.entCache.Len()))
		fs.MetricBlobEntryCount.Set(int64(len(fs.blobCount)))
		fs.MetricBlobEntrySize.Set(int64(fs.blobCache.Size()))
	}()

	if was, ok := fs.entCache.PeekOk(h); ok {
		fs.decrBlobCountLocked(was.blobHash)
	}
	fs.entCache.Set(h, ent)

	fs.blobCount[ent.blobHash]++

	fs.blobCache.Set(ent.blobHash, contents)

	// If we're over our max size, keep looping deleting old entries until we
	// remove enough blobs. Some entries (if they share a blob) won't end up
	// deleting a blob and will only delete a entCache entry.
	max := fs.GetFileCacheSize()
	for fs.blobCache.Size() > max {
		_, v, ok := fs.entCache.DeleteOldest()
		if !ok {
			break
		}
		fs.decrBlobCountLocked(v.blobHash)
	}
}

func setNFSTime(attr *nfs.FileAttribute, t time.Time) {
	attr.Atime = nfs.ToNFSTime(t)
	attr.Mtime = attr.Atime
	attr.Ctime = attr.Atime
}

func (n *NFSHandler) getFileContentsUncached(ctx context.Context, filename string) (data []byte, attr *nfs.FileAttribute, err error) {
	attr = &nfs.FileAttribute{
		Type:     nfs.FileTypeRegular,
		FileMode: 0444, // unless overridden later
		Nlink:    1,
	}
	setNFSTime(attr, store.FakeStaticFileTime)
	hasher := fnv.New64()
	_, _ = hasher.Write([]byte(filename))
	attr.Fileid = hasher.Sum64()

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
	case wkTSGoExtracted:
		return nil, attr, nil
	case statusFile:
		m := n.statusFile()
		setNFSTime(attr, m.fi.ModTime())
		return m.json, attr, nil
	default:
		return nil, nil, os.ErrNotExist
	}

	if ext := mp.CacheDownloadFileExt; ext != "" {
		sp := n.fs.Stats.StartSpan("nfs.OpenFile-ext-" + ext)
		v, err := n.fs.getMetaFileByExt(ctx, mp.ModVersion, ext)
		sp.End(err)
		if err != nil {
			n.fs.logf("Failed to get %s file for %v: %v", mp.CacheDownloadFileExt, mp.ModVersion, err)
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
