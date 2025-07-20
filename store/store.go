package store

import (
	"context"
	"errors"
	"io"
	"os"
)

var ErrCacheMiss = errors.New("cache miss")

// Store defines the cache storage interface for gomodfs.
type Store interface {
	// GetZipRoot returns a handle to the root of a module's zip file for a given version.
	// If the module is not cached, it should return ErrCacheMiss.
	GetZipRoot(context.Context, ModuleVersion) (ModHandle, error)
	GetInfoFile(context.Context, ModuleVersion) ([]byte, error) // or ErrCacheMiss
	GetModFile(context.Context, ModuleVersion) ([]byte, error)  // or ErrCacheMiss
	PutModFile(context.Context, ModuleVersion, []byte) error
	PutInfoFile(context.Context, ModuleVersion, []byte) error

	// Given a ModHandle returned by GetZipRoot, ...

	GetZipHash(context.Context, ModHandle) ([]byte, error) // but _not_ ErrCacheMiss

	// path is  "foo.txt" or "foo/bar.txt"
	GetFile(ctx context.Context, h ModHandle, path string) ([]byte, error)
	StatFile(ctx context.Context, h ModHandle, path string) (_ os.FileMode, size int64, _ error)

	// path is "" for root, else "foo" or "foo/bar" (no trailing slash)
	Readdir(ctx context.Context, h ModHandle, path string) ([]Dirent, error)

	// PutModule populates the store with the given module version data
	// when ErrCacheMiss is returned.
	PutModule(context.Context, ModuleVersion, PutModuleData) (ModHandle, error)
}

type PutModuleData struct {
	InfoFile []byte // the cache/download/foo/bar/@v/v1.23.info file
	ModFile  []byte // the cache/download/foo/bar/@v/v1.23.mod file
	ZipHash  []byte // the cache/download/foo/bar/@v/v1.23.ziphash file

	// Files are the files in the zip file.3
	// Directories are implicit.
	Files []PutFile
}

type PutFile interface {
	// Path returns the path of the file within the zip,
	// relative to the root of the zip file.
	// For example "main.go" or "foo/bar/baz_test.go".
	Path() string
	Mode() os.FileMode // only 0644 or 0755 are valid
	Size() int64       // size of the file in bytes
	Open() (io.ReadCloser, error)
}

type Dirent struct {
	Name string      // bare name, no slashes
	Mode os.FileMode // can be 0644 or 0 (regular files), 0755 (executable files), or fs.ModeDir; no symlinks
	Size int64       // for regular files
}

type ModuleVersion struct {
	Module  string // unescaped, in original case (e.g. "github.com/Foo/bar")
	Version string // unescaped, in original case (e.g. "v1.2.3")
}

type ModHandle interface{}
