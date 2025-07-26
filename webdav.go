package gomodfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/gomodfs/store"
	"golang.org/x/mod/module"
	"golang.org/x/net/webdav"
)

type webdavFS struct {
	fs      *FS
	verbose bool
}

func (d webdavFS) Mkdir(_ context.Context, _ string, _ os.FileMode) error { return os.ErrPermission }
func (d webdavFS) RemoveAll(_ context.Context, _ string) error            { return os.ErrPermission }
func (d webdavFS) Rename(_ context.Context, _, _ string) error            { return os.ErrPermission }

// wdPath is the gomodfs-specific path structure
// for paths received from WebDAV Stat or OpenFile calls.
type wdPath struct {
	// WellKnown, if non-empty, indicates that the path is
	// for a well-known gomodfs path. The only possible value
	// so far is ".gomodfs-status", for the status file.
	WellKnown string

	// NotExist indicates that the path does not exist.
	// This is used for paths that we know operating systems
	// are just checking for, but don't exist in the gomodfs hierarchy.
	NotExist bool

	// CacheDownloadFileExt is one of "mod", "ziphash", "info"
	// if the path is for "cache/download/<module>/@v/<version>.<CacheDownloadFileExt>".
	// If empty, then ModVersion is also populated.
	CacheDownloadFileExt string

	// InZip, if true, means that the path is within the contents of a zip
	// file (i.e. not in "cache/download/" and deep enough in the filesystem
	// to have hit an a directory with an "@" sign, such as
	// "github.com/!azure/azure-sdk-for-go@v16.2.1+incompatible".
	// When true, ModVersion and Path are also populated.
	InZip bool

	// Path is the path within the zip file, if InZip is true.
	// For the root directory in the zip, Path is empty.
	Path string

	// ModVersion is the module version for the path, if yet known.
	// It's populated if CacheDownloadFileExt is non-empty or
	// InZip is true.
	ModVersion store.ModuleVersion
}

// String returns a debug representation of the wdPath for tests.
func (p wdPath) String() string {
	var sb strings.Builder
	sb.WriteByte('{')
	rv := reflect.ValueOf(p)
	for _, st := range reflect.VisibleFields(rv.Type()) {
		f := rv.FieldByIndex(st.Index)
		if f.IsZero() {
			continue
		}
		if sb.Len() > 1 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s:%v", st.Name, f.Interface())
	}
	sb.WriteByte('}')
	return sb.String()
}

// parseWDPath parses a path as received for a WebDAV Stat or OpenFile call
// and returns its godmodfs-specific structure.
func parseWDPath(name string) (ret wdPath) {
	if name == ".gomodfs-status" {
		ret.WellKnown = ".gomodfs-status"
		return
	}
	if strings.HasPrefix(name, ".") {
		// .DS_Store, .Spotlight-V100, ._., .hidden,
		// .metadata_never_index_unless_rootfs, ..metadata_never_index, etc
		ret.NotExist = true
		return
	}
	if dlSuffix, ok := strings.CutPrefix(name, "cache/download/"); ok {
		escMod, escVer, ok := strings.Cut(dlSuffix, "/@v/")
		if !ok {
			// Not enough.
			return
		}
		ext := path.Ext(escVer)
		if ext == "" {
			return
		}
		switch ext {
		case ".mod", ".ziphash", ".info":
			ret.CacheDownloadFileExt = ext[1:]
		default:
			// Not a recognized cache/download file.
			ret.NotExist = true
			return
		}
		escVer = strings.TrimSuffix(escVer, ext)

		ret.ModVersion, ok = parseModVersion(escMod, escVer)
		if !ok {
			ret.NotExist = true
		}
		return
	}

	// Zip path.
	escMod, verAndPath, ok := strings.Cut(name, "@")
	if !ok {
		return
	}
	escVer, path, _ := strings.Cut(verAndPath, "/")

	ret.ModVersion, ok = parseModVersion(escMod, escVer)
	if ok {
		ret.Path = strings.TrimSuffix(path, "/")
		ret.InZip = true
	} else {
		ret.NotExist = true
	}

	return
}

func parseModVersion(escMod, escVer string) (mv store.ModuleVersion, ok bool) {
	var err error
	mv.Module, err = module.UnescapePath(escMod)
	if err != nil {
		return mv, false
	}
	mv.Version, err = module.UnescapeVersion(escVer)
	if err != nil {
		return mv, false
	}
	return mv, true
}

func (d webdavFS) Stat(ctx context.Context, name string) (fi os.FileInfo, retErr error) {
	sp := d.fs.Stats.StartSpan("webdav.Stat")
	defer func() { sp.End(retErr) }()

	if d.verbose {
		defer func() {
			log.Printf("webdavFS.Stat(%q) = %T, %v", name, fi, retErr)
		}()
	}

	dp := parseWDPath(name)
	if dp.NotExist {
		d.fs.Stats.StartSpan("webdav.Stat.NotExist").End(nil)
		return nil, os.ErrNotExist
	}
	if dp.WellKnown != "" {
		return regFileWithSize{name: name, size: 123}, nil
	}
	if ext := dp.CacheDownloadFileExt; ext != "" {
		sp := d.fs.Stats.StartSpan("webdav.Stat-ext-" + ext)
		v, err := d.fs.getMetaFileByExt(ctx, dp.ModVersion, ext)
		sp.End(err)
		if err != nil {
			log.Printf("Failed to get %s file for %v: %v", dp.CacheDownloadFileExt, dp.ModVersion, err)
			return nil, syscall.EIO
		}
		return regFileWithSize{name: name, size: int64(len(v))}, nil
	}

	if !dp.InZip {
		sp := d.fs.Stats.StartSpan("webdav.Stat-not-zip")
		sp.End(nil)
		// Guess we're still building a directory.
		return dirFileInfo{}, nil
	}

	mh, err := d.fs.getZipRoot(ctx, dp.ModVersion)
	if err != nil {
		return nil, err
	}
	spGF := d.fs.Stats.StartSpan("webdav.Stat-GetFile")
	contents, err := d.fs.Store.GetFile(ctx, mh, dp.Path)
	if err != nil {
		if errors.Is(err, store.ErrIsDir) {
			spGF.End(nil)
			return dirFileInfo{}, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			spGF.End(nil)
			return nil, os.ErrNotExist
		}
		log.Printf("Failed to get file %q in zip for %v: %v", dp.Path, dp.ModVersion, err)
		spGF.End(err)
		return nil, err
	}
	spGF.End(nil)
	return regFileWithSize{name: name, size: int64(len(contents))}, nil
}

func newWDFileFromContents(name string, contents []byte) webdav.File {
	sr := io.NewSectionReader(bytes.NewReader(contents), 0, int64(len(contents)))
	return wdRegularFile{
		ReadSeeker: sr,
		Closer:     io.NopCloser(sr),
		size:       int64(len(contents)),
		path:       name,
	}
}

func (d webdavFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (retFile webdav.File, retErr error) {
	sp := d.fs.Stats.StartSpan("webdav.OpenFile")
	defer func() { sp.End(retErr) }()

	if d.verbose {
		defer func() {
			log.Printf("webdavFS.OpenFile(%q, %d) = %T, %v", name, flag, retFile, retErr)
		}()
	}

	// Reject if any write flag is set.
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, os.ErrPermission
	}
	dp := parseWDPath(name)
	if dp.NotExist {
		d.fs.Stats.StartSpan("webdav.Stat.NotExist").End(nil)
		return nil, os.ErrNotExist
	}
	if dp.WellKnown != "" {
		if dp.WellKnown == ".gomodfs-status" {
			panic("TODO: implement .gomodfs-status")
		}
		return nil, os.ErrNotExist
	}
	if ext := dp.CacheDownloadFileExt; ext != "" {
		sp := d.fs.Stats.StartSpan("webdav.OpenFile-ext-" + ext)
		v, err := d.fs.getMetaFileByExt(ctx, dp.ModVersion, ext)
		sp.End(err)
		if err != nil {
			log.Printf("Failed to get %s file for %v: %v", dp.CacheDownloadFileExt, dp.ModVersion, err)
			return nil, syscall.EIO
		}
		return newWDFileFromContents(name, v), nil
	}

	if !dp.InZip {
		d.fs.Stats.StartSpan("webdav.OpenFile-not-zip").End(nil)
		return &emptyDir{fs: d.fs}, nil
	}

	mh, err := d.fs.getZipRoot(ctx, dp.ModVersion)
	if err != nil {
		return nil, err
	}
	spanGF := d.fs.Stats.StartSpan("webdav.OpenFile-GetFile")
	contents, err := d.fs.Store.GetFile(ctx, mh, dp.Path)
	if err != nil {
		if errors.Is(err, store.ErrIsDir) {
			spanRD := d.fs.Stats.StartSpan("webdav.OpenFile-Readdir")
			ents, err := d.fs.Store.Readdir(ctx, mh, dp.Path)
			spanRD.End(err)
			if err != nil {
				spanGF.End(err)
				return nil, err
			}
			spanGF.End(nil)
			return wdDir{ents: ents}, nil
		}
		spanGF.End(err)
		return nil, err
	}
	spanGF.End(nil)
	return newWDFileFromContents(name, contents), nil
}

type wdRegularFile struct {
	io.ReadSeeker
	io.Closer
	size int64
	path string // inside zip
}

func (wd wdRegularFile) Stat() (os.FileInfo, error) {
	return regFileWithSize{name: wd.path, size: wd.size}, nil
}

func (wdRegularFile) Readdir(count int) ([]fs.FileInfo, error) {
	return nil, os.ErrInvalid
}

func (wdRegularFile) Write([]byte) (int, error) {
	return 0, os.ErrInvalid
}

// wdDir is a [webdav.File] implementation that represents a directory
// in the gomodfs store.
type wdDir struct {
	ents []store.Dirent
}

func (wd wdDir) Stat() (os.FileInfo, error) {
	return dirFileInfo{}, nil
}

func (wd wdDir) Close() error                   { return nil }
func (wd wdDir) Read([]byte) (int, error)       { return 0, os.ErrInvalid }
func (wd wdDir) Write([]byte) (int, error)      { return 0, os.ErrInvalid }
func (wd wdDir) Seek(int64, int) (int64, error) { return 0, os.ErrInvalid }
func (wd wdDir) Readdir(count int) ([]fs.FileInfo, error) {
	if count > 0 {
		panic("unexpected; webdav never passes count > 0")
	}
	fis := make([]fs.FileInfo, len(wd.ents))
	for i, ent := range wd.ents {
		if ent.Mode.IsDir() {
			fis[i] = dirFileInfo{}
		} else {
			fis[i] = regFileWithSize{name: ent.Name, size: ent.Size}
		}
	}
	return fis, nil
}

type regFileWithSize struct {
	name string
	size int64
}

func (r regFileWithSize) Name() string       { return r.name }
func (r regFileWithSize) Size() int64        { return r.size }
func (r regFileWithSize) Mode() os.FileMode  { return 0444 }
func (r regFileWithSize) ModTime() time.Time { return time.Time{} }
func (r regFileWithSize) IsDir() bool        { return false }
func (r regFileWithSize) Sys() any           { return nil }

type dirFileInfo struct{}

func (dirFileInfo) Name() string       { return "/" }
func (dirFileInfo) Size() int64        { return 0 }
func (dirFileInfo) Mode() os.FileMode  { return os.ModeDir | 0555 }
func (dirFileInfo) ModTime() time.Time { return time.Time{} }
func (dirFileInfo) IsDir() bool        { return true }
func (dirFileInfo) Sys() any           { return nil }

type emptyDir struct {
	fs *FS

	webdav.File
}

func (ed *emptyDir) Stat() (os.FileInfo, error) {
	return dirFileInfo{}, nil
}

func (ed *emptyDir) Close() error { return nil }

func (ed *emptyDir) Readdir(count int) ([]fs.FileInfo, error) {
	return nil, nil
}
