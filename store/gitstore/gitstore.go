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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/gomodfs/stats"
	"github.com/tailscale/gomodfs/store"
	"go4.org/mem"
	"golang.org/x/mod/module"
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

// getObject looks up the git object with the given rev (a hash or ref or
// rev-parse expression).
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
		if res.err == nil && res.obj.typ != wantType {
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
		select {
		case req.res <- objResponse{obj: object{typ: objType, content: buf, hash: hash}}:
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
	gitHash  [sha1.Size]byte
}

type object struct {
	typ     string // "blob" or "tree"
	content []byte // binary contents of the object, after the "<type> <size>\x00" prefix
	hash    string // optional; hex string of the SHA1 hash of the object
}

type treeBuilder struct {
	d *Storage            // for git commands
	f map[string]fileInfo // file name to contents

	hashSet map[string]object
	hashes  []string // hashes in order of addition (dependencies come first)
}

func newTreeBuilder(d *Storage) *treeBuilder {
	return &treeBuilder{
		d: d,
		f: make(map[string]fileInfo),
	}
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
	s1 := sha1.New()
	fmt.Fprintf(s1, "blob %d\x00", len(all))
	s1.Write(all)

	fi := fileInfo{
		contents: all,
		mode:     mode,
		gitHash:  [sha1.Size]byte(s1.Sum(nil)),
	}
	tb.f[name] = fi
	tb.addHash(fmt.Sprintf("%02x", fi.gitHash), "blob", all)
	return nil
}

// objType is "blob" or "tree".
// contents is the binary contents of the object, before the "<type> <size>\x00" prefix.
func (tb *treeBuilder) addHash(hash string, objType string, contents []byte) {
	if _, ok := tb.hashSet[hash]; ok {
		return
	}
	if tb.hashSet == nil {
		tb.hashSet = make(map[string]object)
	}
	tb.hashSet[hash] = object{typ: objType, content: contents}
	tb.hashes = append(tb.hashes, hash)
}

// prefix is "" or "dir/".
func (tb *treeBuilder) buildTree(dir string) (string, error) {
	var buf bytes.Buffer // of binary tree

	ents := tb.dirEnts(dir)
	for _, ent := range ents {
		mode := "100644"
		if ent.isDir {
			mode = "40000"
		} else if ent.fi.mode&execBits != 0 {
			mode = "100755"
		}
		var entHash string
		if ent.isDir {
			var err error
			entHash, err = tb.buildTree(dir + ent.base + "/")
			if err != nil {
				return "", fmt.Errorf("failed to build sub-tree for %q: %w", ent.base, err)
			}
		} else {
			entHash = fmt.Sprintf("%02x", ent.fi.gitHash)
		}
		binHash, err := hex.DecodeString(entHash)
		if err != nil || len(binHash) != sha1.Size {
			return "", fmt.Errorf("failed to decode hash %q for %q: %w, len=%v", entHash, ent.base, err, len(binHash))
		}
		if strings.ContainsAny(ent.base, "\x00/") {
			return "", fmt.Errorf("invalid file name %q in tree: contains NUL or slash", ent.base)
		}
		fmt.Fprintf(&buf, "%s %s\x00%s", mode, ent.base, binHash)
	}

	s1 := sha1.New()
	fmt.Fprintf(s1, "tree %d\x00%s", buf.Len(), buf.Bytes())
	treeHash := fmt.Sprintf("%02x", s1.Sum(nil))
	tb.addHash(treeHash, "tree", buf.Bytes())
	return treeHash, nil
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
	cmd.Stdin = strings.NewReader(strings.Join(tb.hashes, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("error setting up stdout to check existing git objects: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start git cat-file: %w", err)
	}
	var missing []string
	bs := bufio.NewScanner(stdout)
	missingSuffix := []byte(" missing")
	for bs.Scan() {
		line := bs.Bytes()
		hash, ok := bytes.CutSuffix(line, missingSuffix)
		if !ok {
			continue
		}
		missing = append(missing, string(hash))
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
	modTree string

	mu sync.Mutex
	// ...
	// TODO: pre-load contents
}

func (s *Storage) GetFile(ctx context.Context, h store.ModHandle, path string) ([]byte, error) {
	tree := h.(*modHandle).modTree

	obj, err := s.getObject(ctx, tree+":zip/"+path, "blob")
	if err != nil {
		return nil, err
	}
	return obj.content, nil
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

func (s *Storage) GetZipRoot(ctx context.Context, mv store.ModuleVersion) (store.ModHandle, error) {
	var zero mem.RO
	ref, err := refName(mv)
	if err != nil {
		return zero, err
	}
	out, err := s.getObject(ctx, ref+"^{tree}", "tree")
	if err != nil {
		if errors.Is(err, errMissing) {
			return zero, store.ErrCacheMiss
		}
		return zero, fmt.Errorf("failed to get zip root tree for %q: %w", mv, err)
	}
	if out.hash == "" {
		return zero, fmt.Errorf("gitstore: empty tree hash for %q", mv)
	}
	return &modHandle{modTree: out.hash}, nil
}

func (s *Storage) GetZipHash(ctx context.Context, h store.ModHandle) ([]byte, error) {
	tree := h.(*modHandle).modTree

	out, err := s.getObject(ctx, tree+":ziphash", "blob")
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

	if out, err := s.git("update-ref", ref, treeHash).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to update tree ref %q: %w: %s", ref, err, out)
	}

	return &modHandle{modTree: treeHash}, nil
}

func (s *Storage) Readdir(ctx context.Context, h store.ModHandle, path string) ([]store.Dirent, error) {
	tree := h.(*modHandle).modTree

	out, err := s.git("ls-tree",
		"-t", // include trees
		"--format=%(objectmode) %(objectsize) %(path)",
		tree+":zip/"+path).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list tree %q: %w, %s", tree, err, out)
	}

	sc := bufio.NewScanner(bytes.NewReader(out))
	var ents []store.Dirent
	for sc.Scan() {
		line := sc.Text()
		modeStr, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("unexpected git ls-tree output: %q", line)
		}
		sizeStr, name, ok := strings.Cut(rest, " ")
		if !ok {
			return nil, fmt.Errorf("unexpected git ls-tree output: %q", line)
		}
		if sizeStr == "-" {
			ents = append(ents, store.Dirent{
				Name: name,
				Mode: 0755 | fs.ModeDir, // only ModeDir really matters
			})
			continue
		}
		mode, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("failed to parse mode in line %q", line)
		}
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse size in line %q: %w", line, err)
		}
		ents = append(ents, store.Dirent{
			Name: name,
			Mode: fs.FileMode(mode),
			Size: size,
		})
	}
	return ents, nil
}

func (s *Storage) StatFile(ctx context.Context, h store.ModHandle, path string) (_ os.FileMode, size int64, _ error) {
	panic("TODO")
}
