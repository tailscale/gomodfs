// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"iter"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tailscale/gomodfs/internal/lru"
	"github.com/tailscale/gomodfs/stats"
	"github.com/tailscale/gomodfs/store"
	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/sync/singleflight"
	"tailscale.com/types/result"
	"tailscale.com/util/testenv"
)

const (
	statusFile = ".gomodfs-status"
)

// FS is the gomodfs filesystem.
type FS struct {
	Store store.Store

	Stats  *stats.Stats // or nil if stats are not enabled
	Client *http.Client // or nil to use default client

	sf singleflight.Group

	// ModuleProxyURL is the URL of the Go module proxy to use.
	// If empty, "https://proxy.golang.org" is used.
	// It should not have a trailing slash.
	ModuleProxyURL string

	Logf func(format string, args ...any) // if non-nil, alternate logger to use

	Verbose bool

	// FileCacheSize specifies the file cache size to use.
	// If zero, a default size is used.
	FileCacheSize int64

	// Metrics for Prometheus:

	MetricFileContentCacheHit  expvar.Int `type:"counter" name:"file_content_cache_hit" help:"number of file content cache hits"`
	MetricFileContentCacheMiss expvar.Int `type:"counter" name:"file_content_cache_miss" help:"number of file content cache misses"`
	MetricFileContentCacheFill expvar.Int `type:"counter" name:"file_content_cache_fill" help:"number of file content cache fills"`
	MetricFileEntryCount       expvar.Int `type:"gauge" name:"file_content_entry_count" help:"current number of file entries in the file content cache"`
	MetricBlobEntryCount       expvar.Int `type:"gauge" name:"blob_entry_count" help:"current number of blob entries in the blob cache"`
	MetricBlobEntrySize        expvar.Int `type:"gauge" name:"blob_entry_size" help:"current total size of all blob entries in the blob cache"`

	mu             sync.RWMutex
	zipRootCache   map[store.ModuleVersion]modHandleCacheEntry
	modVerHash     map[modVerHash]store.ModuleVersion // nil until first used
	pathHashTarget map[pathHash]handleTarget          // nil until first used

	statusCache *statusMeta

	// handle maps from an NFS handle to the path it corresponds to.
	//
	// TODO(bradfitz): LRU-ify this. this was once load bearing but it's now a
	// cache now that all handles can map back to a handleTarget anyway
	handle map[handle]handleTarget

	entCache  lru.Cache[handle, *readCacheEntry]
	blobCache lru.Cache[blobHash, []byte]
	blobCount map[blobHash]int // ref count of blobHash in entCache
}

func (fs *FS) GetFileCacheSize() int64 {
	const defaultFileCacheSize = 2 << 30
	return cmp.Or(fs.FileCacheSize, defaultFileCacheSize)
}

func hashModVersion(mv store.ModuleVersion) (ret modVerHash) {
	s := sha256.New()
	io.WriteString(s, mv.Module)
	io.WriteString(s, "\x00")
	io.WriteString(s, mv.Version)
	s.Sum(ret[:0])
	return
}

type modHandleCacheEntry struct {
	h        store.ModHandle
	lastUsed *atomic.Int64 // unix seconds
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

func (fs *FS) netLogf(format string, arg ...any) {
	if testenv.InTest() && fs.Client != nil {
		format = "[fake-network] " + format
	}
	fs.logf(format, arg...)
}

func (fs *FS) logf(format string, arg ...any) {
	if fs.Logf != nil {
		fs.Logf(format, arg...)
	} else {
		log.Printf(format, arg...)
	}
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

	t0 := time.Now()
	defer func() {
		if err != nil && ctx0.Err() == nil {
			fs.logf("netSlurp(%q) failed: %v", urlStr, err)
		} else if err == nil {
			fs.netLogf("downloaded %s (%d bytes) in %v", urlStr, len(ret), time.Since(t0).Round(time.Millisecond))
		}
	}()

	fs.netLogf("starting download of %q ...", urlStr)

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

func (fs *FS) getZipRootCached(mv store.ModuleVersion) (mh store.ModHandle, ok bool) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	ent, ok := fs.zipRootCache[mv]
	if !ok {
		return nil, false
	}
	ent.lastUsed.Store(time.Now().Unix())
	return ent.h, true
}

func (fs *FS) setZipRootCache(mv store.ModuleVersion, h store.ModHandle) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.zipRootCache == nil {
		fs.zipRootCache = make(map[store.ModuleVersion]modHandleCacheEntry)
	}
	if fs.modVerHash != nil {
		// If it's already initialized, add new stuff to it.
		// Otherwise if it's nil, it'll get populated later.
		fs.modVerHash[hashModVersion(mv)] = mv
		fs.addModuleVersionPathsLocked(mv)
	}
	now := new(atomic.Int64)
	now.Store(time.Now().Unix())
	fs.zipRootCache[mv] = modHandleCacheEntry{
		h:        h,
		lastUsed: now,
	}
	if fs.Verbose {
		log.Printf("added zip root for %v to cache", mv)
	}
}

func (fs *FS) getZipRoot(ctx context.Context, mv store.ModuleVersion) (mh store.ModHandle, err error) {
	mh, ok := fs.getZipRootCached(mv)
	if ok {
		return mh, nil
	}

	span := fs.Stats.StartSpan("get-zip-root")
	defer func() { span.End(err) }()

	ctx = context.Background() // TODO(bradfitz): make a singleflight variant that refcounts context lifetime

	rooti, err, _ := fs.sf.Do("get-zip-root:"+mv.Module+"@"+mv.Version, func() (any, error) {
		mh, ok := fs.getZipRootCached(mv)
		if ok {
			return mh, nil
		}

		// Special case for the Tailscale Go toolchain.
		if mv.Module == "github.com/tailscale/go" && strings.HasPrefix(mv.Version, "tsgo-") {
			parts := strings.SplitN(mv.Version, "-", 4)
			if len(parts) != 4 {
				return nil, fmt.Errorf("invalid tsgo version %q", mv.Version)
			}
			goos, goarch, commitHash := parts[1], parts[2], parts[3]
			_, root, err := fs.getTailscaleGoRoot(ctx, goos, goarch, commitHash)
			if err != nil {
				return nil, fmt.Errorf("failed to get Tailscale Go root for %q: %w", mv.Version, err)
			}
			fs.setZipRootCache(mv, root)
			return root, nil
		}

		root, err := fs.Store.GetZipRoot(ctx, mv)
		if err == nil {
			fs.setZipRootCache(mv, root)
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

		fs.setZipRootCache(mv, root)
		return root, nil
	})
	if err != nil {
		return nil, err
	}
	return rooti.(store.ModHandle), nil
}

// ext is one of "mod", "ziphash", "info".
func (fs *FS) getMetaFileByExt(ctx context.Context, mv store.ModuleVersion, ext string) ([]byte, error) {
	switch ext {
	case "mod":
		return fs.getModFile(ctx, mv)
	case "ziphash":
		return fs.getZiphash(ctx, mv)
	case "info":
		return fs.getInfoFile(ctx, mv)
	}
	return nil, fmt.Errorf("unknown meta file extension %q", ext)
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

func (fs *FS) walkStoreModulePaths(ctx context.Context, mh store.ModHandle) iter.Seq[result.Of[string]] {
	return func(yield func(result.Of[string]) bool) {
		var doDir func(string) bool // recursive dir walker, called with "" (root) or "path/to/file-or-dir"
		doDir = func(path string) bool {
			if !yield(result.Value(path)) {
				return false
			}
			ents, err := fs.Store.Readdir(ctx, mh, path)
			if err != nil {
				yield(result.Error[string](err))
				return false
			}
			for _, ent := range ents {
				sub := ent.Name
				if path != "" {
					sub = path + "/" + ent.Name
				}
				if ent.Mode.IsDir() {
					if !doDir(sub) {
						return false
					}
				} else {
					if !yield(result.Value(sub)) {
						return false
					}
				}
			}
			return true
		}
		doDir("")
	}
}

// modVerHash is SHA256(store.ModuleVersion).
type modVerHash [sha256.Size]byte

func (h modVerHash) IsZero() bool { return h == modVerHash{} }

// pathHash is SHA256(either abs path or path-within-a-zip)
type pathHash [sha256.Size]byte

var (
	cdFileInfo    = mkWellKnownPathHash("info")
	cdFileMod     = mkWellKnownPathHash("mod")
	cdFileZiphash = mkWellKnownPathHash("ziphash")
	pathHashTSGo  = mkWellKnownPathHash(wkTSGoExtracted)
)

func mkWellKnownPathHash(s string) pathHash {
	var h pathHash
	copy(h[:], s)
	return h
}

// returns (v, nil) on hit, (zero, nil) on miss, or (zero, err) on error.
func (fs *FS) moduleVersionWithHash(mvh modVerHash) (mv store.ModuleVersion, err error) {
	var zero store.ModuleVersion

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.initHandleMapsLocked(); err != nil {
		fs.logf("initHandleMapsLocked error: %v", err)
		return zero, err
	}
	return fs.modVerHash[mvh], nil
}

func (fs *FS) initHandleMapsLocked() error {
	if fs.modVerHash != nil {
		return nil
	}

	mvs, err := fs.Store.CachedModules(context.TODO())
	if err != nil {
		return fmt.Errorf("CachedModules: %w", err)
	}

	fs.modVerHash = make(map[modVerHash]store.ModuleVersion)
	fs.pathHashTarget = make(map[pathHash]handleTarget)

	for _, mv := range mvs {
		fs.modVerHash[hashModVersion(mv)] = mv
		fs.addModuleVersionPathsLocked(mv)
	}

	for _, path := range []string{
		"cache",
		"cache/download",
		statusFile,
	} {
		fs.addPathHashTargetLocked(path)
	}
	for _, goos := range tsGoGeese {
		for _, goarch := range tsGoGoarches {
			fs.addPathHashTargetLocked(fmt.Sprintf("tsgo-%s-%s", goos, goarch))
		}
	}

	return nil
}

// handleTargetWithPathHash maps from a NFS path hash
//
// should return [staleErr] on non-I/O-error-related miss.
func (fs *FS) handleTargetWithPathHash(ph pathHash) (handleTarget, error) {
	var zero handleTarget

	fs.mu.Lock()
	defer fs.mu.Unlock()

	if err := fs.initHandleMapsLocked(); err != nil {
		return zero, err
	}

	ht, ok := fs.pathHashTarget[ph]
	if !ok {
		return zero, staleErr(fmt.Errorf("unknown path hash %02x", ph))
	}
	return ht, nil
}

func (fs *FS) addModuleVersionPathsLocked(mv store.ModuleVersion) {
	// Make "cache/download/github.com/foo/bar/@v/v1.2.3.foo" and then walk up
	// its directories to add them to the cache. We don't care about the "foo"
	// final component; just the directories above it.
	fs.addParentPathsLocked(cdPath(mv, "foo"))

	// Now add the zip root directories above the one with the "@" in it.
	// e.g. "github.com/!azure/azure-sdk-for-go/sdk" components above
	// "github.com/!azure/azure-sdk-for-go/sdk/azcore@v1.11.0"
	path, err := modVerZipPrefix(mv)
	if err == nil {
		fs.addParentPathsLocked(path)
	} else {
		fs.logf("failed to get modVerZipPrefix for %v: %v", mv, err)
	}
}

func (fs *FS) addParentPathsLocked(s string) {
	for {
		s = path.Dir(s) // not filepath; we want forward slashes even on Windows
		switch s {
		case ".", "cache", "cache/download":
			// Stop once you see any of these.
			// They're all well-known already, so we're done.
			return
		}
		fs.addPathHashTargetLocked(s)
	}
}

func (fs *FS) addPathHashTargetLocked(path string) {
	fs.pathHashTarget[mkPathHash(path)] = handleTarget{
		path: path,
		segs: splitSegs(path),
	}
}

var procStart = time.Now()

type procStat struct {
	Filesystem string                   `json:"filesystem"`
	Uptime     float64                  `json:"uptime"` // seconds since process start
	Ops        map[string]*stats.OpStat `json:"ops,omitzero"`
}

// cdPath returns the "cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.foo" file.
// It returns an invalid path if mv is invalid or malformed.
func cdPath(mv store.ModuleVersion, ext string) string {
	escMod, _ := module.EscapePath(mv.Module)
	escVer, _ := module.EscapeVersion(mv.Version)
	return "cache/download/" + escMod + "/@v/" + escVer + "." + ext
}

func joinSegs(s []string) string {
	return strings.Join(s, "/")
}

func splitSegs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

func modVerZipPrefix(mv store.ModuleVersion) (string, error) {
	if trip, ok := isTSGoModule(mv); ok {
		return tsGoZipRoot(trip), nil
	}
	modEsc, err := module.EscapePath(mv.Module)
	if err != nil {
		return "", err
	}
	verEsc, err := module.EscapeVersion(mv.Version)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s@%s", modEsc, verEsc), nil
}

var infoTmpRx = regexp.MustCompile(`\.info\d+\.tmp$`)

func (s *FS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Stats.ServeHTTP(w, r)
}

// statusJSON returns the JSON-encoded status
// of the <root>/.gomodfs-status file.
func (f *FS) statusJSON() []byte {
	stj, _ := json.MarshalIndent(procStat{
		Filesystem: "gomodfs",
		Uptime:     time.Since(procStart).Seconds(),
		Ops:        f.Stats.Clone(),
	}, "", "  ")
	stj = append(stj, '\n')
	return stj
}

// MountOpts are options for mounting the gomodfs filesystem.
//
// A nil value is equivalent to the zero value.
type MountOpts struct {
	Debug bool // if true, enables debug logging
}

type FileServer interface {
	Unmount() error
	Wait()
}

func (f *FS) MountWebDAV(mntPoint string, opt *MountOpts) (FileServer, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("gomodfs: WebDAV mount is currently only supported on macOS")
	}
	if opt == nil {
		opt = &MountOpts{}
	}

	// Configure the WebDAV handler.
	ln, err := net.Listen("tcp", "localhost:8793")
	if err != nil {
		ln, err = net.Listen("tcp", "localhost:0")
	}
	if err != nil {
		return nil, fmt.Errorf("gomodfs: failed to listen on port: %w", err)
	}
	log.Printf("gomodfs: webdav listening %s", ln.Addr().String())
	hs := &http.Server{
		Handler: f.newWebDAVHandler(opt.Debug),
	}
	go hs.Serve(ln)

	out, err := exec.Command("/sbin/mount_webdav",
		"-S", // unmount without GUI popups on any problems
		"-v", "gomodfs",
		"http://localhost:"+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port),
		mntPoint).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gomodfs: failed to mount WebDAV: %w; output: %s", err, out)
	}
	log.Printf("gomodfs: mounted WebDAV at %s", mntPoint)

	mt := &webdavMount{ln: ln, path: mntPoint}
	mt.ctx, mt.cancel = context.WithCancel(context.Background())
	return mt, nil
}

func (fs *FS) MountNFS(mntDir string, nfsAddr net.Addr) error {
	port := nfsAddr.(*net.TCPAddr).Port
	ip, ok := netip.AddrFromSlice(nfsAddr.(*net.TCPAddr).IP)
	if !ok {
		return fmt.Errorf("gomodfs: invalid NFS address %q", nfsAddr)
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		ip = netip.MustParseAddr("127.0.0.1")
	}
	var mountBin, mountOpts string
	switch runtime.GOOS {
	case "darwin":
		mountBin = "/sbin/mount"
		mountOpts = darwinNFSMountOpts(port)
	case "linux":
		mountBin = "/usr/bin/mount"
		mountOpts = linuxNFSMountOpts(port)
	default:
		return fmt.Errorf("gomodfs: unsupported OS %q; NFS mount is currently only supported on linux and darwin", runtime.GOOS)
	}
	cmd := exec.Command(mountBin,
		"-t", "nfs",
		"-o", mountOpts,
		fmt.Sprintf("%s:/", ip),
		mntDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gomodfs: failed to run mount command: %w; output: %s", err, out)
	}
	return nil
}

// https://linux.die.net/man/5/nfs
func linuxNFSMountOpts(nfsPort int) string {
	opts := []string{
		// port is port at which NFS service should be available
		// https://datatracker.ietf.org/doc/html/rfc1813#section-3.
		"port=" + fmt.Sprint(nfsPort),
		// mountport is port at which the mount RPC service should be available
		// https://datatracker.ietf.org/doc/html/rfc1813#section-5.2.
		"mountport=" + fmt.Sprint(nfsPort),
		// version is NFS version. go-nfs implements NFSv3.
		"vers=3",
		"tcp",
		"local_lock=all", // required to pacify cmd/go file locking
		// timeout for NFS operations in deciseconds.
		// 1800 = 3 minutes.
		"timeo=1800",
		// Create a readonly volume.
		"ro",
	}
	return strings.Join(opts, ",")
}

// https://linux.die.net/man/5/nfs
func darwinNFSMountOpts(nfsPort int) string {
	opts := []string{
		// port is port at which NFS service should be available
		// https://datatracker.ietf.org/doc/html/rfc1813#section-3.
		"port=" + fmt.Sprint(nfsPort),
		// mountport is port at which the mount RPC service should be available
		// https://datatracker.ietf.org/doc/html/rfc1813#section-5.2.
		"mountport=" + fmt.Sprint(nfsPort),
		// version is NFS version. go-nfs implements NFSv3.
		"vers=3",
		"tcp",
		"locallocks", // required to pacify cmd/go file locking
		// timeout for NFS operations in deciseconds.
		// 1800 = 3 minutes.
		"timeo=1800",
		// Create a readonly volume.
		"rdonly",
	}
	return strings.Join(opts, ",")
}

type webdavMount struct {
	path string
	ln   net.Listener

	cancel context.CancelFunc
	ctx    context.Context
}

func (mt *webdavMount) Unmount() error {
	mt.cancel()
	if err := mt.ln.Close(); err != nil {
		return fmt.Errorf("gomodfs: failed to close WebDAV listener: %w", err)
	}
	out, err := exec.Command("/sbin/umount", mt.path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gomodfs: failed to unmount WebDAV: %w; %s", err, out)
	}
	return nil
}

func (mt *webdavMount) Wait() {
	<-mt.ctx.Done()
}
