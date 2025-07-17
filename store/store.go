package store

import (
	"context"
	"errors"
	"io"
	"os"

	"go4.org/mem"
)

var ErrCacheMiss = errors.New("cache miss")

// Store defines the cache storage interface for gomodfs.
type Store interface {
	// GetZipRoot returns a handle to the root of a module's zip file for a given version.
	// If the module is not cached, it should return ErrCacheMiss.
	GetZipRoot(context.Context, ModuleVersion) (ModHandle, error)
	GetInfo(context.Context, ModuleVersion) (mem.RO, error) // or ErrCacheMiss
	GetMod(context.Context, ModuleVersion) (mem.RO, error)  // or ErrCacheMiss

	// Given a ModHandle returned by GetZipRoot, ...

	GetZipHash(context.Context, ModHandle) (mem.RO, error) // but _not_ ErrCacheMiss

	// path is  "foo.txt" or "foo/bar.txt"
	GetFile(ctx context.Context, h ModHandle, path string) (mem.RO, error)
	StatFile(ctx context.Context, h ModHandle, path string) (_ os.FileMode, size int64, _ error)

	// path is "" for root, else "foo" or "foo/bar" (no trailing slash)
	Readdir(ctx context.Context, h ModHandle, path string) ([]Dirent, error)

	// PutModule populates the store with the given module version data
	// when ErrCacheMiss is returned.
	PutModule(context.Context, ModuleVersion, PutModuleData) error
}

type PutModuleData struct {
	InfoFile mem.RO // the cache/download/foo/bar/@v/v1.23.info file
	ModFile  mem.RO // the cache/download/foo/bar/@v/v1.23.mod file
	ZipHash  mem.RO // the cache/download/foo/bar/@v/v1.23.ziphash file

	// Files are the files in the zip file.
	// Directories are implicit.
	Files []PutFile
}

type PutFile interface {
	// Path returns the path of the file within the zip,
	// relative to the root of the zip file.
	// For example "main.go" or "foo/bar/baz_test.go".
	Path() string
	Size() int64 // size of the file in bytes
	Open() (io.ReadCloser, error)
}

type Dirent struct {
	Name string      // bare name, no slashes
	Mode os.FileMode // can be 0644 or 0 (regular files), 0755 (executable files), or fs.ModeDir; no symlinks
	Size int64       // for regular files; -1 if unknown (will require extra fetches)
}

type ModuleVersion struct {
	Module  string // unescaped, in original case (e.g. "github.com/Foo/bar")
	Version string // unescaped, in original case (e.g. "v1.2.3")
}

type ModHandle interface{}
