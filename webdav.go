// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
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

func (f *FS) newWebDAVHandler(debug bool) http.Handler {
	wh := &webdav.Handler{
		Prefix: "/",
		FileSystem: webdavFS{
			fs:      f,
			verbose: debug,
		},
		LockSystem: webdav.NewMemLS(), // shouldn't be needed; but required to be non-nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if debug {
			log.Printf("webdav %s %s", r.Method, r.URL.Path)
		}
		sp := f.Stats.StartSpan("webdav.ServeHTTP." + r.Method)
		defer sp.End(nil) // TODO: track errors too

		// If if the request is a conditional content request (GET or HEAD),
		// just says it's always not modified because all our content is immutable.
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if r.Header.Get("If-Modified-Since") != "" || r.Header.Get("If-None-Match") != "" {
				f.Stats.StartSpan("webdav.NotModified").End(nil)
				w.WriteHeader(http.StatusNotModified)
				return
			}

			// For GET/HEAD requests (for file content, not for directory listings),
			// set a long Expires header.
			expires := time.Now().UTC().AddDate(0, 3, 0) // 3 months? sure.
			w.Header().Set("Expires", expires.Format(http.TimeFormat))
		}

		wh.ServeHTTP(w, r)
	})
}

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
	if strings.HasPrefix(name, "tsgo-") {
		return parseWDTSGoPath(name)
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

func parseWDTSGoPath(name string) (ret wdPath) {
	name, ok := strings.CutPrefix(name, "tsgo-")
	if !ok {
		return
	}
	osHyphenArch, rest, ok := strings.Cut(name, "/")
	if !ok {
		return
	}
	goos, goarch, ok := strings.Cut(osHyphenArch, "-")
	if !ok {
		return
	}
	if !validTSGoOSARCH(goos, goarch) {
		ret.NotExist = true
		return
	}
	firstSeg, rest, _ := strings.Cut(rest, "/")
	m := tsgoRootLookupRx.FindStringSubmatch(firstSeg)
	if m == nil {
		ret.NotExist = true
		return
	}
	hash, wantExtractedFile := m[1], m[2] != ""

	ret.ModVersion.Module = "github.com/tailscale/go"
	ret.ModVersion.Version = fmt.Sprintf("tsgo-%s-%s-%s", goos, goarch, hash)
	if wantExtractedFile {
		ret.WellKnown = "tsgo.extracted"
		return
	}
	ret.Path = strings.TrimSuffix(rest, "/")
	ret.InZip = true
	return ret
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
	base := path.Base(name)
	sp := d.fs.Stats.StartSpan("webdav.Stat")
	defer func() {
		spErr := retErr
		if errors.Is(spErr, os.ErrNotExist) {
			spErr = nil // don't count not-found errors
		}
		sp.End(spErr)
	}()

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
		return regFileInfo{name: name, size: 123}, nil
	}
	if ext := dp.CacheDownloadFileExt; ext != "" {
		sp := d.fs.Stats.StartSpan("webdav.Stat-et-" + ext)
		v, err := d.fs.getMetaFileByExt(ctx, dp.ModVersion, ext)
		sp.End(err)
		if err != nil {
			log.Printf("Failed to get %s file for %v: %v", dp.CacheDownloadFileExt, dp.ModVersion, err)
			return nil, syscall.EIO
		}
		return regFileInfo{name: name, size: int64(len(v))}, nil
	}

	if !dp.InZip {
		sp := d.fs.Stats.StartSpan("webdav.Stat-not-zip")
		sp.End(nil)
		// Guess we're still building a directory.
		return dirFileInfo{baseName: base}, nil
	}

	mh, err := d.fs.getZipRoot(ctx, dp.ModVersion)
	if err != nil {
		return nil, err
	}
	spSS := d.fs.Stats.StartSpan("webdav.Stat.Store.Stat")
	fi, err = d.fs.Store.Stat(ctx, mh, dp.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			spSS.End(nil)
			return nil, os.ErrNotExist
		}
		log.Printf("Failed to get file %q in zip for %v: %v", dp.Path, dp.ModVersion, err)
		spSS.End(err)
		return nil, err
	}
	spSS.End(nil)
	return fi, nil
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
	base := path.Base(name)
	sp := d.fs.Stats.StartSpan("webdav.OpenFile")
	spOK := false
	defer func() {
		if spOK {
			sp.End(nil)
		} else {
			sp.End(retErr)
		}
	}()

	if d.verbose {
		defer func() {
			log.Printf("webdavFS.OpenFile(%q, %d) = %T, %v", name, flag, retFile, retErr)
		}()
	}

	// Reject if any write flag is set.
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		spOK = true // don't treat span as an error
		return nil, os.ErrPermission
	}
	dp := parseWDPath(name)
	if dp.NotExist {
		d.fs.Stats.StartSpan("webdav.Stat.NotExist").End(nil)
		return nil, os.ErrNotExist
	}
	if dp.WellKnown != "" {
		switch dp.WellKnown {
		case ".gomodfs-status":
			return newWDFileFromContents(base, d.fs.statusJSON()), nil
		case "tsgo.extracted":
			return newWDFileFromContents(base, nil), nil
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
		return &emptyDir{fs: d.fs, baseName: base}, nil
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
			return wdDir{pathInZip: dp.Path, baseName: base, ents: ents}, nil
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
	return regFileInfo{name: wd.path, size: wd.size}, nil
}

func (wdRegularFile) Readdir(count int) ([]fs.FileInfo, error) {
	return nil, os.ErrInvalid
}

func (wdRegularFile) Write([]byte) (int, error) {
	return 0, os.ErrInvalid
}

var creationDateProp = xml.Name{Space: "DAV:", Local: "creationdate"}

var (
	fakeStaticFileTime = store.FakeStaticFileTime
)

var staticDeadProps = map[xml.Name]webdav.Property{
	creationDateProp: {
		XMLName:  creationDateProp,
		InnerXML: []byte("<D:creationdate xmlns:D=\"DAV:\">" + fakeStaticFileTime.Format(time.RFC3339) + "</D:creationdate>"),
	},
}

// Implement DeadPropsHolder so we can set a creation date, which macOS maybe
// reportedly expects? It at least asks for it.
var _ webdav.DeadPropsHolder = wdRegularFile{}

func (wd wdRegularFile) DeadProps() (map[xml.Name]webdav.Property, error) {
	return staticDeadProps, nil
}
func (wd wdRegularFile) Patch([]webdav.Proppatch) ([]webdav.Propstat, error) {
	return nil, webdav.ErrNotImplemented
}

// wdDir is a [webdav.File] implementation that represents a directory
// in the gomodfs store.
type wdDir struct {
	pathInZip string // the path within the zip file, for debug logs
	baseName  string
	ents      []store.Dirent
}

func (wd wdDir) Stat() (os.FileInfo, error) {
	return dirFileInfo{baseName: wd.baseName}, nil
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
			fis[i] = dirFileInfo{baseName: ent.Name}
		} else {
			fis[i] = regFileInfo{name: ent.Name, size: ent.Size, mode: ent.Mode}
		}
	}
	return fis, nil
}

// Implement DeadPropsHolder so we can set a creation date, which macOS maybe
// reportedly expects? It at least asks for it.
var _ webdav.DeadPropsHolder = wdDir{}

func (wdDir) DeadProps() (map[xml.Name]webdav.Property, error) {
	return staticDeadProps, nil
}
func (wdDir) Patch([]webdav.Proppatch) ([]webdav.Propstat, error) {
	return nil, webdav.ErrNotImplemented
}

type regFileInfo struct {
	name string
	size int64
	mode os.FileMode
}

func (r regFileInfo) Name() string       { return r.name }
func (r regFileInfo) Size() int64        { return r.size }
func (r regFileInfo) Mode() os.FileMode  { return cmp.Or(r.mode, 0444) }
func (r regFileInfo) ModTime() time.Time { return fakeStaticFileTime }
func (r regFileInfo) IsDir() bool        { return false }
func (r regFileInfo) Sys() any           { return nil }

type dirFileInfo struct {
	baseName string
}

func (d dirFileInfo) Name() string {
	if d.baseName == "" {
		panic("dirFileInfo.Name called with empty baseName")
	}
	return d.baseName
}
func (dirFileInfo) Mode() os.FileMode  { return os.ModeDir | 0555 }
func (dirFileInfo) Size() int64        { return 0 }
func (dirFileInfo) ModTime() time.Time { return fakeStaticFileTime }
func (dirFileInfo) IsDir() bool        { return true }
func (dirFileInfo) Sys() any           { return nil }

type emptyDir struct {
	fs *FS

	baseName string
	webdav.File
}

func (ed *emptyDir) Stat() (os.FileInfo, error) {
	return dirFileInfo{baseName: ed.baseName}, nil
}

func (ed *emptyDir) Close() error { return nil }

func (ed *emptyDir) Readdir(count int) ([]fs.FileInfo, error) {
	return nil, nil
}

var _ webdav.DeadPropsHolder = (*emptyDir)(nil)

func (ed *emptyDir) DeadProps() (map[xml.Name]webdav.Property, error) {
	return staticDeadProps, nil
}

func (ed *emptyDir) Patch([]webdav.Proppatch) ([]webdav.Propstat, error) {
	return nil, webdav.ErrNotImplemented
}
