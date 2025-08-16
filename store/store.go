package store

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

var ErrCacheMiss = errors.New("cache miss")

var ErrIsDir = errors.New("is a directory")

// Store defines the cache storage interface for gomodfs.
type Store interface {
	// CachedModules returns all the ModuleVersions currently known,
	// even if they're not fully downloaded.
	CachedModules(context.Context) ([]ModuleVersion, error)

	// GetZipRoot returns a handle to the root of a module's zip file for a given version.
	// If the module is not cached, it should return ErrCacheMiss.
	GetZipRoot(context.Context, ModuleVersion) (ModHandle, error)
	GetInfoFile(context.Context, ModuleVersion) ([]byte, error) // or ErrCacheMiss
	GetModFile(context.Context, ModuleVersion) ([]byte, error)  // or ErrCacheMiss
	PutModFile(context.Context, ModuleVersion, []byte) error
	PutInfoFile(context.Context, ModuleVersion, []byte) error

	// Given a ModHandle returned by GetZipRoot, ...

	GetZipHash(context.Context, ModHandle) ([]byte, error) // but _not_ ErrCacheMiss

	// GetFile returns the contents of a file within the zip file.
	// If the file does not exist, it should return os.ErrNotExist.
	// If the file is a directory, it should return ErrIsDir.
	//
	// The path is  "foo.txt" or "foo/bar.txt or "" for the root directory.
	GetFile(ctx context.Context, h ModHandle, path string) ([]byte, error)

	// Stat returns the os.FileInfo for a file within the zip file.
	//
	// The FileInfo's Mode is the mode bits (e.g. 0444, 0644, 0755) and optionally
	// also [os.ModeDir] for directories.
	//
	// If the file does not exist, it should return error [os.ErrNotExist].
	// Other errors are effectively I/O errors.
	Stat(ctx context.Context, h ModHandle, path string) (fs.FileInfo, error)

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
	Mode() fs.FileMode // only 0644 or 0755 are valid
	Size() int64       // size of the file in bytes
	Open() (io.ReadCloser, error)
}

type Dirent struct {
	Name string      // bare name, no slashes
	Mode fs.FileMode // can be 0644 or 0 (regular files), 0755 (executable files), or fs.ModeDir; no symlinks
	Size int64       // for regular files
}

type ModuleVersion struct {
	Module  string // unescaped, in original case (e.g. "github.com/Foo/bar")
	Version string // unescaped, in original case (e.g. "v1.2.3")
}

type ModHandle interface{}

// FakeStaticFileTime is a fake modtime and creation time
// used for immutable static files in the gomodfs filesystem.
var FakeStaticFileTime = time.Date(2009, 11, 12, 13, 14, 15, 0, time.UTC)
