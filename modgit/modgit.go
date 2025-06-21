package modgit

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"

	"golang.org/x/mod/module"
)

type Downloader struct {
	Client *http.Client // or nil to use default client

	GitRepo string
}

type Result struct {
	Tree       string // hash of the git tree
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
	out, err := d.git("rev-parse", refName(mod)+"^{tree}").Output()
	if err == nil {
		return &Result{
			Tree:       strings.TrimSpace(string(out)),
			Downloaded: false,
		}, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "https://proxy.golang.org/"+escMod+"/@v/"+version+".zip", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %q: %w", mod, err)
	}
	res, err := d.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download %q: %w", mod, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download %q: %s", mod, res.Status)
	}
	slurp, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read %q: %w", mod, err)
	}
	log.Printf("Downloaded %d bytes", len(slurp))

	return d.addToGit(modAtVersion, slurp)
}

func (d *Downloader) addToGit(modAtVersion string, data []byte) (*Result, error) {

	// Unzip data to git
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("failed to unzip module data: %w", err)
	}

	prefix := modAtVersion + "/"
	tb := newTreeBuilder(d)
	for _, f := range zr.File {
		suf := f.Name
		suf = strings.TrimPrefix(suf, prefix)
		if err := tb.addFile(suf, f); err != nil {
			return nil, fmt.Errorf("failed to add file %q: %w", suf, err)
		}
		log.Printf("Adding %q to git", suf)
	}

	treeHash, err := tb.buildTree("")
	if err != nil {
		return nil, fmt.Errorf("failed to build git tree: %w", err)
	}

	if out, err := d.git("update-ref", refName(modAtVersion), treeHash).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to update ref %q: %w: %s", modAtVersion, err, out)
	}

	return &Result{
		Tree:       treeHash,
		Downloaded: true,
	}, nil
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

func (tb *treeBuilder) addFile(suf string, f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("failed to open file %q in zip: %w", f.Name, err)
	}
	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("failed to read file %q in zip: %w", f.Name, err)
	}
	tb.f[suf] = fileInfo{
		contents: all,
		mode:     f.Mode(),
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

func refName(module string) string {
	return "refs/gomod/" + url.PathEscape(module)
}
