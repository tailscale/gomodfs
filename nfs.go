package gomodfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"path"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/tailscale/gomodfs/temp-dev-fork/willscott/go-nfs"
)

func (fs *FS) NFSHandler() nfs.Handler {
	// Create a new NFS handler that uses the gomodfs file system.
	return &NFSHandler{fs: fs}
}

type NFSHandler struct {
	fs *FS

	nfs.Handler // temporary embedding during dev to watch what panics
}

var _ nfs.Handler = (*NFSHandler)(nil)

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

// Create creates the named file with mode 0666 (before umask), truncating
// it if it already exists. If successful, methods on the returned File can
// be used for I/O; the associated file descriptor has mode O_RDWR.

func (b billyFS) Create(filename string) (billy.File, error) {
	return nil, errReadonly

}

const helloFileContents = "Hello, gomodfs!\n"

type billyFile struct {
	name string
	*io.SectionReader
}

func (f billyFile) Name() string            { return f.name }
func (billyFile) Close() error              { return nil }
func (billyFile) Lock() error               { return errReadonly }
func (billyFile) Unlock() error             { return errReadonly }
func (billyFile) Truncate(int64) error      { return errReadonly }
func (billyFile) Write([]byte) (int, error) { return 0, errReadonly }

func newBillyFileFromBytes(name string, data []byte, mode os.FileMode) billy.File {
	sr := io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))
	return billyFile{
		name:          name,
		SectionReader: sr,
	}
}

func (b billyFS) Open(filename string) (billy.File, error) {
	log.Printf("gomodfs NFS Open called with %q", filename)
	if filename == "hello-gomodfs.txt" {
		return newBillyFileFromBytes(filename, []byte(helloFileContents), 0444), nil
	}
	return nil, fmt.Errorf("gomodfs TODO open %q", filename)
}

func (b billyFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	if flag != 0 {
		log.Printf("gomodfs NFS OpenFile called with flag %d, but gomodfs is read-only", flag)
		return nil, errReadonly
	}
	return b.Open(filename)
}

func (b billyFS) Stat(filename string) (os.FileInfo, error) {
	panic(fmt.Sprintf("TODO billy Stat(%q)", filename))
}

func (b billyFS) Rename(oldpath, newpath string) error {
	return errReadonly
}

func (b billyFS) Remove(filename string) error {
	return errReadonly
}

func (b billyFS) TempFile(dir, prefix string) (billy.File, error) {
	return nil, errReadonly
}

func (b billyFS) Chroot(path string) (billy.Filesystem, error) {
	panic(fmt.Sprintf("TODO billy Chroot(%q)", path))
}

func (b billyFS) Root() string {
	log.Printf("BillyFS NFS Root called")
	return "/"
}

func (b billyFS) ReadDir(path string) ([]os.FileInfo, error) {
	log.Printf("NFS ReadDir(%q)", path)
	if path != "" {
		return nil, nil
	}
	ret := []os.FileInfo{
		regFileInfo{
			name: "hello-gomodfs.txt",
			size: int64(len(helloFileContents)),
			mode: 0444,
		},
	}
	return ret, nil
}

func (b billyFS) MkdirAll(filename string, perm os.FileMode) error {
	return errReadonly
}

func (b billyFS) Lstat(filename string) (os.FileInfo, error) {
	log.Printf("NFS Lstat(%q)", filename)
	if filename == "" {
		return dirFileInfo{}, nil
	}
	if strings.HasPrefix(filename, ".") {
		return nil, os.ErrNotExist
	}
	if filename == "hello-gomodfs.txt" {
		return regFileInfo{
			name: "hello-gomodfs.txt",
			size: int64(len(helloFileContents)),
			mode: 0444,
		}, nil
	}
	log.Printf("TODO: NFS Lstat(%q) = NotExist for now", filename)

	return nil, os.ErrNotExist // TODO

	panic(fmt.Sprintf("TODO billy Lstat(%q)", filename))
}

func (b billyFS) Symlink(target, link string) error {
	return errReadonly
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
	log.Printf("NFS ToHandle called with fs=%v, path=%v", fs, path)
	if len(path) == 0 {
		return []byte("root")
	}
	if len(path) == 1 && path[0] == "hello-gomodfs.txt" {
		return []byte("hello-test")
	}
	return nil

}

func (h *NFSHandler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	log.Printf("NFS FromHandle called with fh=%q", fh)
	if string(fh) == "root" {
		return billyFS{fs: h.fs}, nil, nil
	}
	if string(fh) == "hello-test" {
		return billyFS{fs: h.fs}, []string{"hello-gomodfs.txt"}, nil
	}
	log.Printf("TODO: NFS FromHandle called with fh=%q", fh)
	return nil, nil, errors.New("TODO: implement FromHandle")
}

func (h *NFSHandler) InvalidateHandle(fs billy.Filesystem, fh []byte) error {
	log.Printf("NFS InvalidateHandle called with fs=%v, fh=%q", fs, fh)
	return nil
}

func (h *NFSHandler) HandleLimit() int {
	return math.MaxInt
}
