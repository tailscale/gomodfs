// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// staticTime is reported for immutable checkout files.
var staticTime = time.Date(2009, 11, 12, 13, 14, 15, 0, time.UTC)

// checkout is an immutable, shallow filesystem rooted at one commit.
// Its Git metadata supports single-revision reads, not history, tags or remotes.
// The index assumes tracked files are unchanged, so this view isn't suitable
// for writable overlays.
type checkout struct {
	repo   *repository
	commit string

	mu       sync.Mutex
	treeEnts map[string][]dirent
	index    []byte
}

// dirent is an entry in an immutable checkout directory.
type dirent struct {
	name string
	mode fs.FileMode
	size int64
	sha  string
}

func (c *checkout) gitDirEnts(p string) ([]dirent, error) {
	switch p {
	case ".git":
		return []dirent{
			{
				name: "HEAD",
				mode: 0444,
				size: 41,
			},
			{
				name: "config",
				mode: 0444,
				size: int64(len(checkoutConfig)),
			},
			{
				name: "index",
				mode: 0444,
			},
			{
				name: "objects",
				mode: fs.ModeDir | 0555,
			},
			{
				name: "refs",
				mode: fs.ModeDir | 0555,
			},
			{
				name: "shallow",
				mode: 0444,
				size: 41,
			},
		}, nil
	case ".git/objects":
		ret := []dirent{
			{
				name: "info",
				mode: fs.ModeDir | 0555,
			},
			{
				name: "pack",
				mode: fs.ModeDir | 0555,
			},
		}
		for i := range 256 {
			ret = append(ret, dirent{
				name: fmt.Sprintf("%02x", i),
				mode: fs.ModeDir | 0555,
			})
		}
		return ret, nil
	case ".git/refs":
		return []dirent{
			{
				name: "heads",
				mode: fs.ModeDir | 0555,
			},
		}, nil
	case ".git/objects/info", ".git/objects/pack", ".git/refs/heads":
		return nil, nil
	}
	if strings.HasPrefix(p, ".git/objects/") && len(strings.TrimPrefix(p, ".git/objects/")) == 2 {
		return nil, nil // object fanout is intentionally not enumerable
	}
	return nil, os.ErrNotExist
}

// readDir returns the entries in directory p.
func (c *checkout) readDir(ctx context.Context, p string) ([]dirent, error) {
	p, ok := cleanPath(p)
	if !ok {
		return nil, os.ErrNotExist
	}
	if p == ".git" || strings.HasPrefix(p, ".git/") {
		return c.gitDirEnts(p)
	}
	c.mu.Lock()
	if e, ok := c.treeEnts[p]; ok {
		ret := append([]dirent(nil), e...)
		c.mu.Unlock()
		return ret, nil
	}
	c.mu.Unlock()
	spec := c.commit + "^{tree}"
	if p != "" {
		spec = c.commit + ":" + p
		// A gitlink appears as a directory in an uninitialized worktree, but
		// its object is a commit rather than a tree and must not be traversed.
		if typ, err := c.repo.command(ctx, "cat-file", "-t", spec).Output(); err == nil && strings.TrimSpace(string(typ)) == "commit" {
			return nil, nil
		}
	}
	out, err := c.repo.command(ctx, "ls-tree", "-z", spec).Output()
	if err != nil {
		return nil, os.ErrNotExist
	}
	var ret []dirent
	for line := range bytes.SplitSeq(out, []byte{0}) {
		if len(line) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(line, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("malformed ls-tree output")
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			return nil, fmt.Errorf("malformed ls-tree metadata %q", meta)
		}
		size := int64(0)
		mode := fileMode(f[0])
		if !mode.IsDir() {
			size = -1 // resolved lazily; tree objects don't contain blob sizes
		}
		ret = append(ret, dirent{
			name: string(name),
			mode: mode,
			size: size,
			sha:  f[2],
		})
	}
	if p == "" {
		ret = append(ret, dirent{
			name: ".git",
			mode: fs.ModeDir | 0555,
		})
	}
	c.mu.Lock()
	c.treeEnts[p] = append([]dirent(nil), ret...)
	c.mu.Unlock()
	return ret, nil
}

func (c *checkout) lookup(ctx context.Context, p string) (dirent, error) {
	p, ok := cleanPath(p)
	if !ok {
		return dirent{}, os.ErrNotExist
	}
	if p == "" {
		return dirent{
			mode: fs.ModeDir | 0555,
		}, nil
	}
	parent, base := path.Split(p)
	parent = strings.TrimSuffix(parent, "/")
	ents, err := c.readDir(ctx, parent)
	if err != nil {
		return dirent{}, err
	}
	for _, e := range ents {
		if e.name == base {
			return e, nil
		}
	}
	// Virtual loose objects are looked up directly and aren't enumerable.
	if obj, ok := objectPath(p); ok {
		data, err := c.looseObject(ctx, obj)
		if err != nil {
			return dirent{}, err
		}
		return dirent{
			name: base,
			mode: 0444,
			size: int64(len(data)),
			sha:  obj,
		}, nil
	}
	return dirent{}, os.ErrNotExist
}

// stat returns metadata for p without following symbolic links.
func (c *checkout) stat(ctx context.Context, p string) (fs.FileInfo, error) {
	p, ok := cleanPath(p)
	if !ok {
		return nil, os.ErrNotExist
	}
	e, err := c.lookup(ctx, p)
	if err != nil {
		return nil, err
	}
	if p == ".git/index" {
		index, err := c.indexFile(ctx)
		if err != nil {
			return nil, err
		}
		e.size = int64(len(index))
	}
	if e.size < 0 {
		out, err := c.repo.command(ctx, "cat-file", "-s", e.sha).Output()
		if err != nil {
			return nil, os.ErrNotExist
		}
		e.size, err = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return nil, err
		}
	}
	return info{
		e:   e,
		mod: staticTime,
	}, nil
}

// readlink returns the target of the symbolic link at p.
func (c *checkout) readlink(ctx context.Context, p string) (string, error) {
	e, err := c.lookup(ctx, p)
	if err != nil {
		return "", err
	}
	if e.mode&fs.ModeSymlink == 0 {
		return "", fs.ErrInvalid
	}
	b, err := c.object(ctx, e.sha)
	return string(b), err
}

const checkoutConfig = `[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
	checkstat = minimal
[gc]
	auto = 0
`

func objectPath(p string) (string, bool) {
	s, ok := strings.CutPrefix(p, ".git/objects/")
	if !ok || len(s) != 41 || s[2] != '/' {
		return "", false
	}
	obj := s[:2] + s[3:]
	return obj, validSHA1(obj)
}

// readFile returns the contents of the regular file at p.
func (c *checkout) readFile(ctx context.Context, p string) ([]byte, error) {
	p, ok := cleanPath(p)
	if !ok {
		return nil, os.ErrNotExist
	}
	switch p {
	case ".git/HEAD", ".git/shallow":
		return []byte(c.commit + "\n"), nil
	case ".git/config":
		return []byte(checkoutConfig), nil
	case ".git/index":
		return c.indexFile(ctx)
	}
	if obj, ok := objectPath(p); ok {
		return c.looseObject(ctx, obj)
	}
	e, err := c.lookup(ctx, p)
	if err != nil {
		return nil, err
	}
	if e.mode.IsDir() {
		return nil, fmt.Errorf("%w: %s", errors.New("is a directory"), p)
	}
	return c.object(ctx, e.sha)
}

func (c *checkout) object(ctx context.Context, sha string) (out []byte, retErr error) {
	defer func() {
		result := "success"
		if retErr != nil {
			result = "error"
		}
		c.repo.mgr.metrics.objectReads.WithLabelValues(c.repo.name, result).Inc()
	}()
	typ, err := c.repo.command(ctx, "cat-file", "-t", sha).Output()
	if err != nil {
		return nil, os.ErrNotExist
	}
	out, err = c.repo.command(ctx, "cat-file", string(bytes.TrimSpace(typ)), sha).Output()
	if err != nil {
		return nil, os.ErrNotExist
	}
	return out, nil
}

// looseObject returns the Git object named by sha in loose-object format.
func (c *checkout) looseObject(ctx context.Context, sha string) ([]byte, error) {
	if !validSHA1(sha) {
		return nil, os.ErrNotExist
	}
	typ, err := c.repo.command(ctx, "cat-file", "-t", sha).Output()
	if err != nil {
		return nil, os.ErrNotExist
	}
	data, err := c.repo.command(ctx, "cat-file", string(bytes.TrimSpace(typ)), sha).Output()
	if err != nil {
		return nil, os.ErrNotExist
	}
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	fmt.Fprintf(zw, "%s %d\x00", bytes.TrimSpace(typ), len(data))
	zw.Write(data)
	zw.Close()
	return b.Bytes(), nil
}

// indexFile returns a synthesized Git index for the checkout tree. It marks
// tracked files assume-unchanged because filling in stat data would require
// fetching every promised blob.
func (c *checkout) indexFile(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.index != nil {
		return append([]byte(nil), c.index...), nil
	}
	f, err := os.CreateTemp(c.repo.dir, "index-*")
	if err != nil {
		return nil, fmt.Errorf("creating checkout index: %w", err)
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Remove(name); err != nil {
		return nil, err
	}
	command := func(args ...string) *exec.Cmd {
		cmd := c.repo.command(ctx, args...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+name, "GIT_INDEX_VERSION=2")
		return cmd
	}
	if err := run(command("read-tree", c.commit+"^{tree}"), "generating checkout index"); err != nil {
		return nil, err
	}
	paths, err := command("ls-files", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("listing checkout index: %w", err)
	}
	// update-index requires a worktree even though marking entries doesn't
	// inspect it. All paths come from our temporary index, not the host.
	cmd := command("--work-tree=.", "update-index", "--assume-unchanged", "-z", "--stdin")
	cmd.Stdin = bytes.NewReader(paths)
	if err := run(cmd, "marking immutable index entries"); err != nil {
		return nil, err
	}
	idx, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("reading checkout index: %w", err)
	}
	c.index = idx
	return append([]byte(nil), idx...), nil
}

type info struct {
	e   dirent
	mod time.Time
}

func (i info) Name() string       { return i.e.name }
func (i info) Size() int64        { return i.e.size }
func (i info) Mode() fs.FileMode  { return i.e.mode }
func (i info) ModTime() time.Time { return i.mod }
func (i info) IsDir() bool        { return i.e.mode.IsDir() }
func (i info) Sys() any           { return nil }

func validSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

func cleanPath(p string) (string, bool) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", true
	}
	if strings.ContainsRune(p, '\x00') || path.Clean(p) != p || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

func fileMode(gitMode string) fs.FileMode {
	switch gitMode {
	case "040000":
		return fs.ModeDir | 0555
	case "100755":
		return 0555
	case "120000":
		return fs.ModeSymlink | 0444
	case "160000":
		return fs.ModeDir | 0555
	default:
		return 0444
	}
}
