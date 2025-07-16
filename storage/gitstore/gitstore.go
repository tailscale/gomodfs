// Package gitstore stores gomodfs modules in a git repository.
//
// git's storage backend has the nice property that it does de-duping even if
// your filesystem doesn't. We don't store commits in git-- only trees and
// blobs. And then refs to trees.
package gitstore

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
)

type Storage struct {
	Client *http.Client // or nil to use default client

	GitRepo string

	// ModuleProxyURL is the URL of the Go module proxy to use.
	// If empty, "https://proxy.golang.org" is used.
	// It should not have a trailing slash.
	ModuleProxyURL string
}

type Result struct {
	ModTree    string // hash of the git tree with "zip" tree, "ziphash"/"info"/"mod" files
	Downloaded bool
}

func (d *Storage) CheckExists() error {
	out, err := d.git("rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%q does not appear to be within a git directory: %v, %s", d.GitRepo, err, out)
	}
	return nil
}

func (d *Storage) moduleProxyURL() string {
	if d.ModuleProxyURL != "" {
		return strings.TrimSuffix(d.ModuleProxyURL, "/")
	}
	return "https://proxy.golang.org"
}

// Get gets a module like "tailscale.com@1.2.43" to the Downloader's
// git repo, either from cache or by downloading it from the Go module proxy.
func (d *Storage) Get(ctx context.Context, modAtVersion string) (*Result, error) {
	mod, version, ok := strings.Cut(modAtVersion, "@")
	if !ok {
		return nil, fmt.Errorf("module %q does not have a version", mod)
	}
	escMod, err := module.EscapePath(mod)
	if err != nil {
		return nil, fmt.Errorf("failed to escape module name %q: %w", mod, err)
	}

	// See if the ref exists first.
	ref := refName(escMod, version)
	treeRef := ref + "^{tree}"
	out, err := d.git("rev-parse", treeRef).Output()
	if err == nil {
		return &Result{
			ModTree:    strings.TrimSpace(string(out)),
			Downloaded: false,
		}, nil
	}

	exts := map[string][]byte{}
	for _, ext := range []string{"zip", "info", "mod"} {
		urlStr := d.moduleProxyURL() + "/" + escMod + "/@v/" + version + "." + ext
		data, err := d.netSlurp(ctx, urlStr)
		if err != nil {
			return nil, fmt.Errorf("failed to download %q: %w", urlStr, err)
		}
		exts[ext] = data
		log.Printf("Downloaded %d bytes from %v", len(data), urlStr)
	}

	return d.addToGit(ref, modAtVersion, exts)
}

func (d *Storage) GetMetaFile(escMod, version, file string) (string, error) {
	ref := refName(escMod, version)
	out, err := d.git("show", ref+":"+file).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get %q for %q: %w, %s", file, ref, err, out)
	}
	return string(out), nil
}

func (d *Storage) GetZipRootTree(modTreeHash string) (string, error) {
	out, err := d.git("rev-parse", modTreeHash+":zip").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get zip hash for %q: %w, %s", modTreeHash, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

var wordRx = regexp.MustCompile(`^\w+$`)

// TreeOfRef returns the tree hash of the given ref, or ("", false) if it
// doesn't exist.
func (d *Storage) GetTailscaleGo(goos, goarch, commit string) (tree string, err error) {
	for _, v := range []string{goos, goarch, commit} {
		if !wordRx.MatchString(v) {
			return "", fmt.Errorf("invalid value %q", v)
		}
	}

	ref := fmt.Sprintf("refs/tsgo-%s-%s-%s", goos, goarch, commit)

	// See if the ref exists first.
	out, err := d.git("rev-parse", ref+"^{tree}").CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}

	// If it doesn't exist, download the tarball.
	urlStr := fmt.Sprintf("https://github.com/tailscale/go/releases/download/build-%s/%s-%s.tar.gz", commit, goos, goarch)
	log.Printf("Downloading %q", urlStr)
	data, err := d.netSlurp(context.Background(), urlStr)
	if err != nil {
		return "", fmt.Errorf("failed to download %q: %w", urlStr, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("failed to gunzip tarball: %w", err)
	}

	tb := newTreeBuilder(d)

	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break // end of tar
		}
		if err != nil {
			return "", fmt.Errorf("failed to read tar header: %w", err)
		}
		if h.FileInfo().IsDir() {
			continue
		}
		name := strings.TrimPrefix(h.Name, "go/")
		if err := tb.addFile(name, func() (io.ReadCloser, error) {
			return io.NopCloser(tr), nil
		}, h.FileInfo().Mode()); err != nil {
			return "", fmt.Errorf("failed to add file %q from tar: %w", h.Name, err)
		}
		log.Printf("added %q to git tree", name)
	}
	treeHash, err := tb.buildTree("")
	if err != nil {
		log.Printf("git tree build error for %v: %v", ref, err)
		return "", fmt.Errorf("failed to build git tree: %w", err)
	}
	if _, err := tb.sendToGit(); err != nil {
		log.Printf("git sendToGit error for %v: %v", ref, err)
	}

	if out, err := d.git("update-ref", ref, treeHash).CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to update tree ref %q: %w: %s", ref, err, out)
	}

	return treeHash, nil
}

func (d *Storage) addToGit(ref, modAtVersion string, parts map[string][]byte) (*Result, error) {
	log.Printf("Adding %s to git ...", modAtVersion)

	// Unzip data to git
	zr, err := zip.NewReader(bytes.NewReader(parts["zip"]), int64(len(parts["zip"])))
	if err != nil {
		return nil, fmt.Errorf("failed to unzip module data: %w", err)
	}

	prefix := modAtVersion + "/"
	tb := newTreeBuilder(d)

	fileNames := make([]string, 0, len(zr.File))
	zipOfFile := map[string]*zip.File{}

	for _, f := range zr.File {
		fileNames = append(fileNames, f.Name)
		zipOfFile[f.Name] = f

		name := f.Name
		name = strings.TrimPrefix(name, prefix)
		if err := tb.addFile("zip/"+name, f.Open, f.Mode()); err != nil {
			return nil, fmt.Errorf("failed to add file %q: %w", name, err)
		}
	}

	zipHash, err := dirhash.Hash1(fileNames, func(name string) (io.ReadCloser, error) {
		f := zipOfFile[name]
		if f == nil {
			return nil, fmt.Errorf("file %q not found in zip", name) // should never happen
		}
		return f.Open()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to hash zip contents: %w", err)
	}

	// Add the metadata files.
	if err := tb.addFile("ziphash", func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(zipHash)), nil
	}, 0644); err != nil {
		return nil, fmt.Errorf("failed to add ziphash file: %w", err)
	}

	// Add the info and mod files.
	for name, contents := range parts {
		if name == "zip" {
			continue // already added
		}
		if err := tb.addFile(name, func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(contents)), nil
		}, 0644); err != nil {
			return nil, fmt.Errorf("failed to add file %q: %w", name, err)
		}
	}

	treeHash, err := tb.buildTree("")
	if err != nil {
		return nil, fmt.Errorf("failed to build git tree: %w", err)
	}
	if _, err := tb.sendToGit(); err != nil {
		log.Printf("git sendToGit error for %v: %v", ref, err)
	}

	if out, err := d.git("update-ref", ref, treeHash).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to update tree ref %q: %w: %s", ref, err, out)
	}

	return &Result{
		ModTree:    treeHash,
		Downloaded: true,
	}, nil
}

func (d *Storage) netSlurp(ctx0 context.Context, urlStr string) (ret []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx0, 30*time.Second)
	defer cancel()

	defer func() {
		if err != nil && ctx0.Err() == nil {
			log.Printf("netSlurp(%q) failed: %v", urlStr, err)
		}
		if err == nil {
			log.Printf("netSlurp(%q) succeeded; %d bytes", urlStr, len(ret))
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %q: %w", urlStr, err)
	}
	res, err := d.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download %q: %w", urlStr, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download %q: %s", urlStr, res.Status)
	}
	return io.ReadAll(res.Body)
}

type fileInfo struct {
	contents []byte
	mode     os.FileMode
	gitHash  [sha1.Size]byte
}

type object struct {
	typ     string // "blob" or "tree"
	content []byte // binary contents of the object, after the "<type> <size>\x00" prefix
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

func (d *Storage) client() *http.Client {
	return cmp.Or(d.Client, http.DefaultClient)
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
func refName(escModuleName, version string) string {
	return "refs/gomod/" + url.PathEscape(escModuleName) + "@" + url.PathEscape(version)
}
