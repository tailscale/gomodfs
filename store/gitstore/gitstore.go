// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package gitstore stores gomodfs modules in a git repository.
//
// git's storage backend has the nice property that it does de-duping even if
// your filesystem doesn't. We don't store commits in git-- only trees and
// blobs. And then refs to trees.
package gitstore

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/gomodfs/stats"
	"github.com/tailscale/gomodfs/store"
	"go4.org/mem"
	"golang.org/x/mod/module"
	"tailscale.com/util/set"
)

type Storage struct {
	Stats   *stats.Stats // or nil if stats are not enabled
	GitRepo string

	mu                  sync.Mutex
	wake                chan bool // 1-buffered best effort wake chan
	pendReq             []objRequest
	penReqArray         [16]objRequest // slice backing array upon reset to length 0
	catFileBatchRunning bool
}

// objRef is a git hash to a commit, tree, blob, etc.
type objRef [sha1.Size]byte

// String returns the hex representation of the object reference.
func (r objRef) String() string { return fmt.Sprintf("%02x", r[:]) }

// CheckExists checks that the git repo named in d.GitRepo actually exists.
func (d *Storage) CheckExists() error {
	out, err := d.git("rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%q does not appear to be within a git directory: %v, %s", d.GitRepo, err, out)
	}
	return nil
}

var errMissing = errors.New("gitstore: missing object")

type objRequest struct {
	rev string           // a git ref or hash
	res chan objResponse // 1-buffered
}

type objResponse struct {
	err error
	obj object // valid only if err == nil
}

// anyType is a sentinel value for getObject to indicate that any type is acceptable
// and the caller will check it.
const anyType = "_anytype_"

// getObject looks up the git object with the given rev (a hash or ref or
// rev-parse expression).
//
// If wantType is [anyType], it doesn't validate the type of the object.
//
// err is errMissing if the object is not found.
func (d *Storage) getObject(ctx context.Context, rev, wantType string) (object, error) {
	req := objRequest{
		rev: rev,
		res: make(chan objResponse, 1),
	}
	d.startRequest(req)

	select {
	case <-ctx.Done():
		return object{}, ctx.Err()
	case res := <-req.res:
		if res.err == nil && (wantType != anyType && res.obj.typ != wantType) {
			return object{}, fmt.Errorf("expected %q (%v) to be a %q, got %q", rev, res.obj.hash, wantType, res.obj.typ)
		}
		return res.obj, res.err
	}
}

func (d *Storage) startRequest(req objRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.wake == nil {
		d.wake = make(chan bool, 1)
	}
	d.pendReq = append(d.pendReq, req)
	select {
	case d.wake <- true:
	default:
	}

	if !d.catFileBatchRunning {
		d.catFileBatchRunning = true
		go d.runCatFileBatch()
	}
}

func (d *Storage) noteCatFileBatchDone(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.pendReq) == 0 {
		go d.runCatFileBatch()
		select {
		case d.wake <- true:
		default:
		}
	} else {
		d.catFileBatchRunning = false
	}
}

func (d *Storage) runCatFileBatch() (err error) {
	sp := d.Stats.StartSpan("gitstore.cat-file-batch")
	defer func() {
		sp.End(err)
		if err != nil {
			log.Printf("gitstore: error running cat-file batch: %v", err)
		}
		d.noteCatFileBatchDone(err)
	}()
	cmd := d.git("cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin setup: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout setup: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting cat-file batch: %w", err)
	}

	defer cmd.Process.Wait()
	defer stdin.Close()

	br := bufio.NewReader(stdout)
	for {
		req, ok := d.takePendingRequest(10 * time.Second)
		if !ok {
			return nil
		}
		sp := d.Stats.StartSpan("gitstore.cat-file-batch.request")
		sendErr := func(err error) error {
			sp.End(err)
			req.res <- objResponse{err: err}
			return err
		}
		if _, err := fmt.Fprintf(stdin, "%s\n", req.rev); err != nil {
			return sendErr(fmt.Errorf("error writing to cat-file batch stdin: %w", err))
		}

		line, err := br.ReadBytes('\n')
		if err != nil {
			return sendErr(fmt.Errorf("error reading cat-file header line: %w", err))
		}

		f := strings.Fields(string(line))

		if len(f) < 2 {
			return sendErr(fmt.Errorf("unexpected cat-file batch output format: %q", line))
		}
		hash, objType := f[0], f[1]
		if objType == "missing" {
			req.res <- objResponse{err: errMissing}
			continue
		}
		if len(f) != 3 {
			return sendErr(fmt.Errorf("unexpected cat-file batch output format w/o 3 fields: %q", line))
		}
		sizeStr := f[2]
		size, err := strconv.Atoi(sizeStr)
		if err != nil {
			return sendErr(fmt.Errorf("error parsing size %q in cat-file batch output: %w", sizeStr, err))
		}
		const maxObjSize = 512 << 20 // 512 MB seems sufficiently large for CI source
		if size < 0 || size > maxObjSize {
			return sendErr(fmt.Errorf("invalid size %d in cat-file batch output: %q", size, line))
		}
		buf := make([]byte, size+1)
		if _, err := io.ReadFull(br, buf); err != nil {
			return sendErr(fmt.Errorf("error reading cat-file batch output: %w", err))
		}
		if buf[size] != '\n' {
			return sendErr(fmt.Errorf("cat-file batch output not terminated with newline: %q", buf[size:]))
		}
		buf = buf[:size]
		var ref objRef
		if _, err := hex.Decode(ref[:], []byte(hash)); err != nil {
			return sendErr(fmt.Errorf("error decoding hash %q in cat-file batch output: %w", hash, err))
		}
		select {
		case req.res <- objResponse{obj: object{typ: objType, content: buf, hash: ref}}:
			sp.End(nil)
		default:
			panic("unexpected cat-file batch response channel full")
		}
	}
}

func (d *Storage) takePendingRequest(timeout time.Duration) (req objRequest, ok bool) {
	for {
		req, wake, ok := d.tryTakePendingRequest()
		if ok {
			return req, true
		}
		select {
		case <-wake:
		case <-time.After(timeout):
			return objRequest{}, false
		}
	}
}

func (d *Storage) tryTakePendingRequest() (req objRequest, wake <-chan bool, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.wake == nil {
		d.wake = make(chan bool, 1)
	}

	if len(d.pendReq) > 0 {
		req = d.pendReq[0]
		d.pendReq = d.pendReq[1:]
		if len(d.pendReq) == 0 {
			d.pendReq = d.penReqArray[:0] // reset slice to empty, but keep backing array
		}
		return req, d.wake, true
	}
	return objRequest{}, d.wake, false
}

type fileInfo struct {
	contents []byte
	mode     os.FileMode
	gitHash  objRef
}

type object struct {
	typ     string // "blob" or "tree"
	content []byte // binary contents of the object, after the "<type> <size>\x00" prefix
	hash    objRef
}

type treeBuilder struct {
	d *Storage            // for git commands
	f map[string]fileInfo // file name to contents

	hashSet map[objRef]object
	hashes  []objRef // hashes in order of addition (dependencies come first)
}

func newTreeBuilder(d *Storage) *treeBuilder {
	return &treeBuilder{
		d: d,
		f: make(map[string]fileInfo),
	}
}

func mkObjRef(typ string, contents []byte) objRef {
	s1 := sha1.New()
	fmt.Fprintf(s1, "%s %d\x00", typ, len(contents))
	s1.Write(contents)
	var ref objRef
	s1.Sum(ref[:0])
	return ref
}

func (tb *treeBuilder) addFile(name string, open func() (io.ReadCloser, error), mode os.FileMode) error {
	rc, err := open()
	if err != nil {
		return fmt.Errorf("failed to open file %q in zip: %w", name, err)
	}
	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("failed to read file %q in zip: %w", name, err)
	}
	blobRef := mkObjRef("blob", all)

	fi := fileInfo{
		contents: all,
		mode:     mode,
		gitHash:  blobRef,
	}
	tb.f[name] = fi
	tb.addHash(blobRef, "blob", all)
	return nil
}

// objType is "blob" or "tree".
// contents is the binary contents of the object, before the "<type> <size>\x00" prefix.
func (tb *treeBuilder) addHash(hash objRef, objType string, contents []byte) {
	if _, ok := tb.hashSet[hash]; ok {
		return
	}
	if tb.hashSet == nil {
		tb.hashSet = make(map[objRef]object)
	}
	var zero objRef
	if hash == zero {
		panic("addHash called with zero hash")
	}
	tb.hashSet[hash] = object{typ: objType, content: contents, hash: hash}
	tb.hashes = append(tb.hashes, hash)
}

// prefix is "" or "dir/".
func (tb *treeBuilder) buildTree(dir string) (objRef, error) {
	var buf bytes.Buffer // of binary tree

	ents := tb.dirEnts(dir)
	for _, ent := range ents {
		if strings.ContainsAny(ent.base, "\x00/") {
			return objRef{}, fmt.Errorf("invalid file name %q in tree: contains NUL or slash", ent.base)
		}
		mode := "100644"
		if ent.isDir {
			mode = "40000"
		} else if ent.fi.mode&execBits != 0 {
			mode = "100755"
		}
		var entHash objRef
		if ent.isDir {
			var err error
			entHash, err = tb.buildTree(dir + ent.base + "/")
			if err != nil {
				return objRef{}, fmt.Errorf("failed to build sub-tree for %q: %w", ent.base, err)
			}
		} else {
			entHash = ent.fi.gitHash
		}
		fmt.Fprintf(&buf, "%s %s\x00", mode, ent.base)
		buf.Write(entHash[:])
	}

	treeRef := mkObjRef("tree", buf.Bytes())
	tb.addHash(treeRef, "tree", buf.Bytes())
	return treeRef, nil
}

type sendToGitStats struct {
	Trees     int
	TreeBytes int64
	Blobs     int
	BlobBytes int64
}

// sendToGit sends all trees & blobs to git.
func (tb *treeBuilder) sendToGit() (*sendToGitStats, error) {
	st := &sendToGitStats{}
	cmd := tb.d.git("cat-file", "--batch-check")
	var chkBuf bytes.Buffer
	for _, hash := range tb.hashes {
		fmt.Fprintf(&chkBuf, "%s\n", hash)
	}
	cmd.Stdin = &chkBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("error setting up stdout to check existing git objects: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start git cat-file: %w", err)
	}
	var missing []objRef
	bs := bufio.NewScanner(stdout)
	missingSuffix := []byte(" missing")
	for bs.Scan() {
		line := bs.Bytes()
		hash, ok := bytes.CutSuffix(line, missingSuffix)
		if !ok {
			continue
		}
		var ref objRef
		if _, err := hex.Decode(ref[:], hash); err != nil {
			return nil, fmt.Errorf("error decoding hash %q in cat-file batch output: %w", hash, err)
		}
		missing = append(missing, ref)
	}
	if err := bs.Err(); err != nil {
		cmd.Process.Kill()
		go cmd.Wait()
		return nil, fmt.Errorf("failed to read git cat-file output: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git cat-file command failed: %w", err)
	}
	if len(missing) == 0 {
		return st, nil
	}

	pw := newPackWriter(len(missing))

	for _, hash := range missing {
		obj, ok := tb.hashSet[hash]
		if !ok {
			return nil, fmt.Errorf("missing hash %q not found in treeBuilder", hash)
		}
		switch obj.typ {
		case "blob":
			st.Blobs++
			st.BlobBytes += int64(len(obj.content))
		case "tree":
			st.Trees++
			st.TreeBytes += int64(len(obj.content))
		default:
			return nil, fmt.Errorf("unknown object type %q for hash %q", obj.typ, hash)
		}

		if err := pw.writePackObject(obj); err != nil {
			return nil, fmt.Errorf("failed to write pack object for %q: %w", hash, err)
		}
	}

	if err := pw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close pack writer: %w", err)
	}

	packFile := pw.buf.Bytes()

	cmd = tb.d.git("index-pack", "--stdin", "--fix-thin", "--strict")
	cmd.Stdin = bytes.NewReader(packFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to run git index-pack: %w, %s", err, out)
	}

	return st, nil
}

type packWriter struct {
	s1     hash.Hash
	closed bool
	buf    bytes.Buffer
}

func newPackWriter(numObjs int) *packWriter {
	pw := &packWriter{
		s1: sha1.New(),
	}
	hdr := make([]byte, 0, 12)
	hdr = append(hdr, "PACK"...)
	hdr = binary.BigEndian.AppendUint32(hdr, 2) // version 2
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(numObjs))
	pw.Write(hdr)
	return pw
}

func (pw *packWriter) writePackObject(obj object) error {
	hdrBuf := make([]byte, 0, 8)
	size := len(obj.content) // XXX
	const (
		ObjTree = 2
		ObjBlob = 3
	)
	var typ byte
	switch obj.typ {
	case "tree":
		typ = ObjTree
	case "blob":
		typ = ObjBlob
	default:
		panic("unreachable")
	}

	firstSizeBits := size & 0x0F
	size >>= 4

	const continueBit = uint8(0x80) // if set, more bytes follow

	firstByte := byte((typ&0x7)<<4) | byte(firstSizeBits)
	if size != 0 {
		firstByte |= continueBit
	}
	hdrBuf = append(hdrBuf, firstByte)
	for size != 0 {
		b := byte(size & 0x7F)
		size >>= 7
		if size != 0 {
			b |= continueBit
		}
		hdrBuf = append(hdrBuf, b)
	}

	pw.Write(hdrBuf)

	zw := zlib.NewWriter(pw)
	if _, err := zw.Write(obj.content); err != nil {
		return fmt.Errorf("failed to write pack object content: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("failed to close zlib writer: %w", err)
	}
	return nil
}

func (pw *packWriter) Close() error {
	if pw.s1 == nil {
		return fmt.Errorf("packWriter already closed")
	}
	// Write the SHA1 of the pack.
	pw.buf.Write(pw.s1.Sum(nil))
	pw.closed = true
	return nil
}

func (pw *packWriter) Write(p []byte) (n int, err error) {
	if pw.closed {
		return 0, fmt.Errorf("packWriter already closed")
	}
	n, err = pw.buf.Write(p)
	if err != nil {
		return n, err
	}
	pw.s1.Write(p)
	return n, nil
}

const execBits = 0111

type dirEnt struct {
	base    string
	sortKey string // either base or base + "/" if isDir
	isDir   bool
	fi      fileInfo
}

// prefix is "" or "dir/".
func (tb *treeBuilder) dirEnts(prefix string) []dirEnt {
	names := map[string]dirEnt{}
	for filename, fi := range tb.f {
		suf, ok := strings.CutPrefix(filename, prefix)
		if !ok {
			continue
		}
		base, _, ok := strings.Cut(suf, "/")
		newEnt := dirEnt{base: base, isDir: ok, fi: fi, sortKey: base}
		if newEnt.isDir {
			// Undocumented git rule: directories have an implicit final "/"
			// at the end of their name before sorting. Otherwise git fsck
			// or git index-pack --strict will fail to accept the pack file.
			newEnt.sortKey += "/"
		}
		if was, ok := names[base]; ok {
			if (was.isDir || newEnt.isDir) && (was.isDir != newEnt.isDir) {
				panic(fmt.Sprintf("unexpected change from %+v to %+v", was, newEnt))
			}
		} else {
			names[base] = newEnt
		}
	}
	ents := slices.Collect(maps.Values(names))
	slices.SortFunc(ents, func(a, b dirEnt) int {
		return cmp.Compare(a.sortKey, b.sortKey)
	})

	return ents
}

func (d *Storage) git(args ...string) *exec.Cmd {
	allArgs := []string{
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
	}
	allArgs = append(allArgs, args...)
	c := exec.Command("git", allArgs...)
	c.Dir = d.GitRepo
	return c
}

// escModuleName is like "!foo!bar.com" form (for FooBar.com)
func refName(h store.ModuleVersion) (string, error) {
	escModuleName, err := module.EscapePath(h.Module)
	if err != nil {
		return "", fmt.Errorf("failed to escape module name %q: %w", h.Module, err)
	}
	escVersion, err := module.EscapeVersion(h.Version)
	if err != nil {
		return "", fmt.Errorf("failed to escape version %q: %w", h.Version, err)
	}
	return "refs/gomod/" + url.PathEscape(escModuleName) + "@" + url.PathEscape(escVersion), nil
}

func modRefName(h store.ModuleVersion) (string, error) {
	base, err := refName(h)
	if err != nil {
		return "", err
	}
	return base + "(mod)", nil
}

func infoRefName(h store.ModuleVersion) (string, error) {
	base, err := refName(h)
	if err != nil {
		return "", err
	}
	return base + "(info)", nil
}

type modHandle struct {
	modTree objRef

	// blobMeta optionally contains the blob refs for file
	// with the tree. (e.g. "store/gitstore/gitstore.go" => blob)
	// If nil, the caller should fall back to getting the data
	// otherwise.
	// It is used for preloading data that'll probably be needed.
	blobMeta map[string]blobMeta

	dirEnts map[string][]store.Dirent // path to dirEnt
}

// blobMeta is the metadata for a blob in a tree.
type blobMeta struct {
	Blob objRef
	Size int64
	Mode os.FileMode
}

func (s *Storage) GetFile(ctx context.Context, h store.ModHandle, path string) ([]byte, error) {
	mh := h.(*modHandle)

	if _, ok := mh.dirEnts[path]; ok {
		return nil, store.ErrIsDir
	}

	obj, err := s.getObject(ctx, fmt.Sprintf("%s:zip/%s", mh.modTree, path), anyType)
	if err != nil {
		if errors.Is(err, errMissing) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	if obj.typ == "tree" {
		return nil, store.ErrIsDir
	}
	if obj.typ != "blob" {
		return nil, fmt.Errorf("expected %q in %v to be a blob, got %q", path, mh.modTree, obj.typ)
	}
	return obj.content, nil
}

// dirFileInfo is a fs.FileInfo for directories in the git store.
type dirFileInfo struct {
	baseName string
}

func (d dirFileInfo) Name() string       { return d.baseName }
func (d dirFileInfo) Size() int64        { return 0 }
func (d dirFileInfo) Mode() os.FileMode  { return fs.ModeDir | 0755 }
func (d dirFileInfo) ModTime() time.Time { return store.FakeStaticFileTime }
func (d dirFileInfo) IsDir() bool        { return true }
func (d dirFileInfo) Sys() any           { return nil }

// regFileInfo is a fs.FileInfo for regular files in the git store.
type regFileInfo struct {
	baseName string
	mode     os.FileMode
	size     int64
}

func (r regFileInfo) Name() string       { return r.baseName }
func (r regFileInfo) Size() int64        { return r.size }
func (r regFileInfo) Mode() os.FileMode  { return r.mode }
func (r regFileInfo) ModTime() time.Time { return store.FakeStaticFileTime }
func (r regFileInfo) IsDir() bool        { return false }
func (r regFileInfo) Sys() any           { return nil }

func (s *Storage) Stat(ctx context.Context, h store.ModHandle, modPath string) (fs.FileInfo, error) {
	mh := h.(*modHandle)
	if _, ok := mh.dirEnts[modPath]; ok {
		return dirFileInfo{baseName: path.Base(modPath)}, nil
	}
	if meta, ok := mh.blobMeta[modPath]; ok {
		return regFileInfo{
			baseName: path.Base(modPath),
			mode:     meta.Mode,
			size:     meta.Size,
		}, nil
	}
	return nil, os.ErrNotExist
}

func (s *Storage) GetInfoFile(ctx context.Context, mv store.ModuleVersion) (_ []byte, err error) {
	sp := s.Stats.StartSpan("gitstore-GetInfoFile")
	defer func() { sp.End(err) }()

	ref, err := refName(mv)
	if err != nil {
		return nil, err
	}

	obj, err := s.getObject(ctx, ref+":info", "blob")
	if err == nil {
		return obj.content, nil
	}

	// If we don't have the full module downloaded, maybe
	// we have just the info file.
	ref, err = infoRefName(mv)
	if err != nil {
		return nil, err
	}

	obj, err = s.getObject(ctx, ref, "blob")
	if err == nil {
		return obj.content, nil
	}
	if errors.Is(err, errMissing) {
		return nil, store.ErrCacheMiss
	}
	return nil, err
}

func (s *Storage) GetModFile(ctx context.Context, mv store.ModuleVersion) (_ []byte, err error) {
	sp := s.Stats.StartSpan("gitstore-GetModFile")
	defer func() { sp.End(err) }()

	ref, err := modRefName(mv)
	if err != nil {
		return nil, err
	}
	out, err := s.getObject(ctx, ref, "blob")
	if err != nil {
		if errors.Is(err, errMissing) {
			return nil, store.ErrCacheMiss
		}
		return nil, err
	}
	return out.content, nil
}

func (s *Storage) PutModFile(ctx context.Context, mv store.ModuleVersion, data []byte) (err error) {
	sp := s.Stats.StartSpan("gitstore-PutModFile")
	defer func() { sp.End(err) }()

	ref, err := modRefName(mv)
	if err != nil {
		return err
	}
	c := s.git("hash-object", "-w", "--stdin")
	c.Stdin = bytes.NewReader(data)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to hash object for %q: %w, %s", mv, err, out)
	}
	if out, err := s.git("update-ref", ref, strings.TrimSpace(string(out))).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to update mod blob ref %q: %w: %s", ref, err, out)
	}
	return nil
}

func (s *Storage) PutInfoFile(ctx context.Context, mv store.ModuleVersion, data []byte) (err error) {
	sp := s.Stats.StartSpan("gitstore-PutInfoFile")
	defer func() { sp.End(err) }()

	ref, err := infoRefName(mv)
	if err != nil {
		return err
	}
	c := s.git("hash-object", "-w", "--stdin")
	c.Stdin = bytes.NewReader(data)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to hash object for %q: %w, %s", mv, err, out)
	}
	if out, err := s.git("update-ref", ref, strings.TrimSpace(string(out))).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to update info blob ref %q: %w: %s", ref, err, out)
	}
	return nil
}

func fileModeFromGitOctalBytes(octal []byte) os.FileMode {
	switch string(octal) {
	case "040000": // tree
		return 0755 | fs.ModeDir
	case "100644": // blob, regular file
		return 0644
	case "100755": // blob, executable file
		return 0755
	case "120000": // symlink
		return 0755 | fs.ModeSymlink
	case "160000": // commit (git submodule)
		return 0755 | fs.ModeIrregular // sure :)
	}
	return 0
}

func (s *Storage) GetZipRoot(ctx context.Context, mv store.ModuleVersion) (store.ModHandle, error) {
	var zero store.ModHandle
	ref, err := refName(mv)
	if err != nil {
		return zero, err
	}

	treeRev := ref + "^{tree}"
	treeObj, err := s.getObject(ctx, treeRev, "tree")
	if err != nil {
		if errors.Is(err, errMissing) {
			return zero, store.ErrCacheMiss
		}
		return zero, fmt.Errorf("failed to get zip root tree for %v: %w", mv, err)
	}

	return s.newModeHandle(mv, treeObj.hash)
}

func (s *Storage) newModeHandle(mv store.ModuleVersion, modTree objRef) (store.ModHandle, error) {
	var zero store.ModHandle

	mh := &modHandle{
		modTree:  modTree,
		blobMeta: make(map[string]blobMeta),
		dirEnts:  make(map[string][]store.Dirent),
	}

	out, err := s.git("ls-tree", "-t", "-r", "--format=%(objectname) %(objectmode) %(objectsize) %(path)", modTree.String()+":zip").CombinedOutput()
	if err != nil {
		return zero, fmt.Errorf("failed to get zip root tree for %v: %w", mv, err)
	}
	nl, sp := []byte{'\n'}, []byte{' '}
	remain := out
	for len(remain) > 0 {
		line, rest, ok := bytes.Cut(remain, nl)
		if !ok {
			return zero, fmt.Errorf("unexpected git ls-tree output: %q", out)
		}
		remain = rest

		// line is either a directory (size "-") or a file like:
		// 3c5168bddc0916b4335d60dab404a0f7f46b3a86 040000 - cmd
		// 7d58018ed84876cf098948dbb753707c0ff7c90f 100644 900 go.mod

		// column 0: the hex object name
		hexRef, rest, ok := bytes.Cut(line, sp)
		if !ok {
			return zero, fmt.Errorf("unexpected git ls-tree output: %q", line)
		}
		var ref objRef
		if _, err := hex.Decode(ref[:], hexRef); err != nil {
			return zero, fmt.Errorf("failed to decode hex in ls-tree line %q: %w", line, err)
		}

		// column 1: the object mode (040000 for tree, 100644 or 100755 for blob)
		modeOct, rest, ok := bytes.Cut(rest, sp)
		if !ok {
			return zero, fmt.Errorf("unexpected git ls-tree output: %q", line)
		}
		mode := fileModeFromGitOctalBytes(modeOct)

		// column 2: the object size (or "-" for a directory)
		sizeStr, rest, ok := bytes.Cut(rest, sp)
		if !ok || mode.IsDir() && string(sizeStr) != "-" {
			return zero, fmt.Errorf("unexpected git ls-tree output: %q", line)
		}
		var size int64
		if !mode.IsDir() {
			size, err = mem.ParseInt(mem.B(sizeStr), 10, 64)
			if err != nil {
				return zero, fmt.Errorf("failed to parse size %q in ls-tree line %q: %w", sizeStr, line, err)
			}
		}
		pathFromRoot := string(rest)

		if !mode.IsDir() {
			mh.blobMeta[pathFromRoot] = blobMeta{
				Blob: ref,
				Size: size,
				Mode: mode,
			}
		}

		ent := store.Dirent{
			Name: path.Base(pathFromRoot),
			Mode: mode,
			Size: size,
		}

		dir := path.Dir(pathFromRoot)
		if dir == "." {
			dir = ""
		}
		mh.dirEnts[dir] = append(mh.dirEnts[dir], ent)
	}
	return mh, nil
}

func (s *Storage) GetZipHash(ctx context.Context, h store.ModHandle) ([]byte, error) {
	tree := h.(*modHandle).modTree

	out, err := s.getObject(ctx, tree.String()+":ziphash", "blob")
	if err != nil {
		return nil, err
	}
	return out.content, nil
}

func (s *Storage) PutModule(ctx context.Context, mv store.ModuleVersion, data store.PutModuleData) (store.ModHandle, error) {
	ref, err := refName(mv)
	if err != nil {
		return nil, err
	}

	tb := newTreeBuilder(s)
	static := func(v []byte) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(v)), nil
		}
	}
	parts := map[string]*[]byte{
		"ziphash": &data.ZipHash,
		"info":    &data.InfoFile,
		"mod":     &data.ModFile,
	}
	for ext, sptr := range parts {
		if *sptr == nil {
			// Support omitting these for tsgo's (ab)use of PutModule
			// for putting toolchain snapshots. It doesn't need these.
			continue
		}
		if err := tb.addFile(ext, static(*sptr), 0644); err != nil {
			return nil, fmt.Errorf("failed to add %s file: %w", ext, err)
		}
	}
	for _, f := range data.Files {
		if err := tb.addFile("zip/"+f.Path(), f.Open, f.Mode()); err != nil {
			return nil, fmt.Errorf("failed to add zip file %q: %w", f.Path(), err)
		}
	}

	treeHash, err := tb.buildTree("")
	if err != nil {
		return nil, fmt.Errorf("failed to build git tree: %w", err)
	}
	if _, err := tb.sendToGit(); err != nil {
		log.Printf("git sendToGit error for %v: %v", mv, err)
	}

	if out, err := s.git("update-ref", ref, treeHash.String()).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to update tree ref %q: %w: %s", ref, err, out)
	}

	return s.newModeHandle(mv, treeHash)
}

func (s *Storage) Readdir(ctx context.Context, h store.ModHandle, path string) ([]store.Dirent, error) {
	mh := h.(*modHandle)
	return slices.Clone(mh.dirEnts[path]), nil
}

func (s *Storage) CachedModules(ctx context.Context) ([]store.ModuleVersion, error) {
	sp := s.Stats.StartSpan("gitstore-CachedModules")
	defer sp.End(nil)

	cmd := s.git("for-each-ref", "--format=%(refname:short)", "refs/gomod/")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list git refs: %w, %s", err, out)
	}

	var mods []store.ModuleVersion
	seen := set.Set[store.ModuleVersion]{}
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		paren := bytes.IndexByte(line, '(')
		if paren != -1 {
			line = line[:paren] // remove (mod) etc suffix
		}
		rest, ok := strings.CutPrefix(string(line), "gomod/")
		if !ok {
			continue
		}
		modEsc2, verEsc2, ok := strings.Cut(rest, "@")
		if !ok {
			continue
		}
		modEsc, err1 := url.PathUnescape(modEsc2)
		verEsc, err2 := url.PathUnescape(verEsc2)
		if err1 != nil || err2 != nil {
			continue
		}
		mod, err1 := module.UnescapePath(modEsc)
		ver, err2 := module.UnescapeVersion(verEsc)
		if err1 != nil || err2 != nil {
			continue
		}
		mv := store.ModuleVersion{
			Module:  mod,
			Version: ver,
		}
		if seen.Contains(mv) {
			continue
		}
		seen.Add(mv)
		mods = append(mods, mv)
	}
	return mods, nil
}
