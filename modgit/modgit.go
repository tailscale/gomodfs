package modgit

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"fmt"
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

type Downloader struct {
	Client *http.Client // or nil to use default client

	GitRepo string
}

type Result struct {
	ModTree    string // hash of the git tree with "zip" tree, "ziphash"/"info"/"mod" files
	Downloaded bool
}

func (d *Downloader) CheckExists() error {
	out, err := d.git("rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%q does not appear to be within a git directory: %v, %s", d.GitRepo, err, out)
	}
	return nil
}

// Get gets a module like "tailscale.com@1.2.43" to the Downloader's
// git repo, either from cache or by downloading it from the Go module proxy.
func (d *Downloader) Get(ctx context.Context, modAtVersion string) (*Result, error) {
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
		urlStr := "https://proxy.golang.org/" + escMod + "/@v/" + version + "." + ext
		data, err := d.netSlurp(ctx, urlStr)
		if err != nil {
			return nil, fmt.Errorf("failed to download %q: %w", urlStr, err)
		}
		exts[ext] = data
		log.Printf("Downloaded %d bytes from %v", len(data), urlStr)
	}

	return d.addToGit(ref, modAtVersion, exts)
}

func (d *Downloader) GetMetaFile(escMod, version, file string) (string, error) {
	ref := refName(escMod, version)
	out, err := d.git("show", ref+":"+file).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get %q for %q: %w, %s", file, ref, err, out)
	}
	return string(out), nil
}

func (d *Downloader) GetZipRootTree(modTreeHash string) (string, error) {
	out, err := d.git("rev-parse", modTreeHash+":zip").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get zip hash for %q: %w, %s", modTreeHash, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

var wordRx = regexp.MustCompile(`^\w+$`)

// TreeOfRef returns the tree hash of the given ref, or ("", false) if it
// doesn't exist.
func (d *Downloader) GetTailscaleGo(goos, goarch, commit string) (tree string, err error) {
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
	log.Printf("building git tree ....")
	treeHash, err := tb.buildTree("")
	if err != nil {
		log.Printf("git tree build error for %v: %v", ref, err)
		return "", fmt.Errorf("failed to build git tree: %w", err)
	}

	if out, err := d.git("update-ref", ref, treeHash).CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to update tree ref %q: %w: %s", ref, err, out)
	}

	return treeHash, nil
}

func (d *Downloader) addToGit(ref, modAtVersion string, parts map[string][]byte) (*Result, error) {

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
		log.Printf("Adding %q to git", name)
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

	if out, err := d.git("update-ref", ref, treeHash).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to update tree ref %q: %w: %s", ref, err, out)
	}

	return &Result{
		ModTree:    treeHash,
		Downloaded: true,
	}, nil
}

func (d *Downloader) netSlurp(ctx0 context.Context, urlStr string) (ret []byte, err error) {
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
}

type treeBuilder struct {
	d *Downloader         // for git commands
	f map[string]fileInfo // file name to contents
}

func newTreeBuilder(d *Downloader) *treeBuilder {
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
	tb.f[name] = fileInfo{
		contents: all,
		mode:     mode,
	}
	return nil
}

// prefix is "" or "dir/".
func (tb *treeBuilder) buildTree(dir string) (string, error) {
	var buf bytes.Buffer // of text tree

	ents := tb.dirEnts(dir)
	for _, ent := range ents {
		// <mode> <filename>\0<binary object id>
		typ := "blob"
		mode := "100644"
		if ent.isDir {
			mode = "040000"
			typ = "tree"
		} else {
			if ent.fi.mode&execBits != 0 {
				mode = "100755"
			}
		}
		var entHash string
		if ent.isDir {
			var err error
			entHash, err = tb.buildTree(dir + ent.base + "/")
			if err != nil {
				return "", fmt.Errorf("failed to build sub-tree for %q: %w", ent.base, err)
			}
		} else {
			c := tb.d.git("hash-object", "-w", "--stdin")
			c.Stdin = bytes.NewReader(ent.fi.contents)
			out, err := c.Output()
			if err != nil {
				return "", fmt.Errorf("failed to hash object for %q: %w", ent.base, err)
			}
			entHash = string(bytes.TrimSpace(out))
		}
		fmt.Fprintf(&buf, "%s %s %s\t%s\n", mode, typ, entHash, ent.base)
	}

	c := tb.d.git("mktree")
	c.Stdin = bytes.NewReader(buf.Bytes())
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to mktree: %w: %s\non:\n%s", err, out, buf.Bytes())
	}
	return string(bytes.TrimSpace(out)), nil
}

const execBits = 0111

type dirEnt struct {
	base  string
	isDir bool
	fi    fileInfo
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
		names[base] = dirEnt{base: base, isDir: ok, fi: fi}
	}
	ents := slices.Collect(maps.Values(names))
	slices.SortFunc(ents, func(a, b dirEnt) int {
		return cmp.Compare(a.base, b.base)
	})
	return ents
}

func (d *Downloader) client() *http.Client {
	return cmp.Or(d.Client, http.DefaultClient)
}

func (d *Downloader) AddBlob(r io.Reader) (hash string, err error) {
	c := d.git("hash-object", "-w", "--stdin")
	c.Stdin = r
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to hash object: %w, %s", err, out)
	}
	return string(bytes.TrimSpace(out)), nil
}

func (d *Downloader) AddTreeFromTextFormat(txtTree []byte) (hash string, err error) {
	c := d.git("mktree")
	c.Stdin = bytes.NewReader(txtTree)
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to mktree: %w: %s\non:\n%s", err, out, txtTree)
	}
	return string(bytes.TrimSpace(out)), nil

}

func (d *Downloader) git(args ...string) *exec.Cmd {
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
