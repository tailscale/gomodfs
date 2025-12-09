// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package gomodfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aegistudio/go-winfsp"
	"github.com/aegistudio/go-winfsp/gofs"
	"github.com/tailscale/gomodfs/store"
)

type winFSPRunner struct {
	mfs *FS
	fs  *winfsp.FileSystem
}

func (mfs *FS) MountWinFSP(mntPoint string) (MountRunner, error) {
	fspFS, err := winfsp.Mount(
		gofs.New(&fspFS{fs: mfs}),
		mntPoint, // e.g. "M:"
		winfsp.FileSystemName("gomodfs"),
		winfsp.Attributes(winfsp.FspFSAttributeReadOnlyVolume),
	)
	if err != nil {
		return nil, fmt.Errorf("winfsp.Mount: %w", err)
	}

	s := &winFSPRunner{
		mfs: mfs,
		fs:  fspFS,
	}
	return s, nil
}

// fspFS implements [gofs.FileSystem].
type fspFS struct {
	fs      *FS
	verbose bool
}

var _ gofs.FileSystem = &fspFS{}

func (pfs *fspFS) Mkdir(name string, perm os.FileMode) error {
	return os.ErrPermission // read-only filesystem
}

func (pfs *fspFS) Remove(name string) error {
	return os.ErrPermission // read-only filesystem
}

func (pfs *fspFS) Rename(source, target string) error {
	return os.ErrPermission // read-only filesystem
}

var (
	helloStat os.FileInfo = regFileInfo{
		name: "gomodfs hello.txt",
		size: int64(len(helloMsg)),
	}

	rootStat os.FileInfo = dirFileInfo{baseName: "\\"}
)

func unixifyPath(name string) string {
	// paths come in like "\\" or "\\some file.txt". Change them to forward slashes and
	// no leading slash.
	name = filepath.ToSlash(name)
	name = strings.TrimPrefix(name, "/")
	return name
}

func (pfs *fspFS) Stat(name string) (fi os.FileInfo, retErr error) {
	ctx := context.Background()
	name = unixifyPath(name)

	if name == "" {
		return rootStat, nil
	}
	if name == "gomodfs hello.txt" {
		return helloStat, nil
	}

	d := pfs
	base := path.Base(name)
	sp := d.fs.Stats.StartSpan("fsp.Stat")
	defer func() {
		spErr := retErr
		if errors.Is(spErr, os.ErrNotExist) {
			spErr = nil // don't count not-found errors
		}
		sp.End(spErr)
	}()

	if d.verbose {
		defer func() {
			log.Printf("fsp.Stat(%q) = %T, %v", name, fi, retErr)
		}()
	}

	dp := parsePath(name)
	if dp.NotExist {
		d.fs.Stats.StartSpan("fsp.Stat.NotExist").End(nil)
		return nil, os.ErrNotExist
	}
	if dp.WellKnown != "" {
		return regFileInfo{name: name, size: 123}, nil
	}
	if ext := dp.CacheDownloadFileExt; ext != "" {
		sp := d.fs.Stats.StartSpan("fsp.Stat-et-" + ext)
		v, err := d.fs.getMetaFileByExt(ctx, dp.ModVersion, ext)
		sp.End(err)
		if err != nil {
			log.Printf("Failed to get %s file for %v: %v", dp.CacheDownloadFileExt, dp.ModVersion, err)
			return nil, syscall.EIO
		}
		return regFileInfo{name: name, size: int64(len(v))}, nil
	}

	if !dp.InZip {
		sp := d.fs.Stats.StartSpan("fsp.Stat-not-zip")
		sp.End(nil)
		// Guess we're still building a directory.
		return dirFileInfo{baseName: base}, nil
	}

	mh, err := d.fs.getZipRoot(ctx, dp.ModVersion)
	if err != nil {
		return nil, err
	}
	spSS := d.fs.Stats.StartSpan("fsp.Stat/Store.Stat")
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

var rootDir gofs.File = &dirWithEnts{
	fi: rootStat,
	ents: []os.FileInfo{
		helloStat,
	},
}

func (pfs *fspFS) OpenFile(name string, flag int, perm os.FileMode) (retFile gofs.File, retErr error) {
	if flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0 {
		return nil, os.ErrPermission // read-only filesystem
	}
	name = unixifyPath(name)

	if name == "" {
		return rootDir, nil
	}
	if name == "gomodfs hello.txt" {
		return newFWPFileFromContents(name, []byte(helloMsg)), nil
	}
	ctx := context.Background()

	d := pfs
	base := path.Base(name)
	sp := d.fs.Stats.StartSpan("fsp.OpenFile")
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
			log.Printf("fspFS.OpenFile(%q, %d) = %T, %v", name, flag, retFile, retErr)
		}()
	}

	// Reject if any write flag is set.
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		spOK = true // don't treat span as an error
		return nil, os.ErrPermission
	}
	dp := parsePath(name)
	if dp.NotExist {
		d.fs.Stats.StartSpan("fsp.Stat.NotExist").End(nil)
		return nil, os.ErrNotExist
	}
	if dp.WellKnown != "" {
		switch dp.WellKnown {
		case statusFile:
			return newFWPFileFromContents(base, d.fs.statusJSON()), nil
		case wkTSGoExtracted:
			return newFWPFileFromContents(base, nil), nil
		}
		return nil, os.ErrNotExist
	}
	if ext := dp.CacheDownloadFileExt; ext != "" {
		sp := d.fs.Stats.StartSpan("fsp.OpenFile-ext-" + ext)
		v, err := d.fs.getMetaFileByExt(ctx, dp.ModVersion, ext)
		sp.End(err)
		if err != nil {
			log.Printf("Failed to get %s file for %v: %v", dp.CacheDownloadFileExt, dp.ModVersion, err)
			return nil, syscall.EIO
		}
		return newFWPFileFromContents(name, v), nil
	}

	if !dp.InZip {
		d.fs.Stats.StartSpan("fsp.OpenFile-not-zip").End(nil)
		return &emptyDir{fs: d.fs, baseName: base}, nil
	}

	mh, err := d.fs.getZipRoot(ctx, dp.ModVersion)
	if err != nil {
		return nil, err
	}
	spanGF := d.fs.Stats.StartSpan("fsp.OpenFile-GetFile")
	contents, err := d.fs.Store.GetFile(ctx, mh, dp.Path)
	if err != nil {
		if errors.Is(err, store.ErrIsDir) {
			spanRD := d.fs.Stats.StartSpan("fsp.OpenFile-Readdir")
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
	return newFWPFileFromContents(name, contents), nil
}

func (s *winFSPRunner) Unmount() error {
	panic("TODO: unmount")
}

func (s *winFSPRunner) Wait() {
	for range time.NewTicker(time.Hour).C {
	}
}

type dirWithEnts struct {
	fi        os.FileInfo
	ents      []os.FileInfo
	gofs.File // embed nil pointer to implement but panic on surprise calls
}

func (f *dirWithEnts) Close() error { return nil }

func (f *dirWithEnts) Stat() (os.FileInfo, error) { return f.fi, nil }

func (f *dirWithEnts) Readdir(count int) (ents []os.FileInfo, err error) { return f.ents, nil }

const helloMsg = "Hello from gomodfs on WinFSP!\n"

func newFWPFileFromContents(baseName string, contents []byte) gofs.File {
	return &winFSPRegularFile{
		contents: contents,
		fi: regFileInfo{
			name: baseName,
			size: int64(len(contents)),
		},
	}
}

type winFSPRegularFile struct {
	fi       os.FileInfo
	contents []byte
}

func (f *winFSPRegularFile) Close() error { return nil }

func (f *winFSPRegularFile) Sync() error               { return nil }
func (f *winFSPRegularFile) Truncate(size int64) error { return os.ErrPermission }
func (f *winFSPRegularFile) Write(p []byte) (n int, err error) {
	return 0, os.ErrPermission
}
func (f *winFSPRegularFile) WriteAt(p []byte, off int64) (n int, err error) {
	return 0, os.ErrPermission
}

func (f *winFSPRegularFile) Readdir(count int) (ents []os.FileInfo, err error) {
	return nil, os.ErrInvalid
}

func (f *winFSPRegularFile) Read(p []byte) (n int, err error) {
	panic("unused") // winfsp uses ReadAt only
}

func (f *winFSPRegularFile) ReadAt(p []byte, off int64) (n int, err error) {
	n = copy(p, f.contents[min(off, int64(len(f.contents))):])
	if n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (f *winFSPRegularFile) Seek(offset int64, whence int) (int64, error) {
	panic("unused") // winfsp only uses Seek for Renames, which we don't support (being read-only)
}

func (f *winFSPRegularFile) Stat() (os.FileInfo, error) {
	return f.fi, nil
}
