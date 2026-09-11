// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"bytes"
	"context"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

var testEnv = append(os.Environ(),
	"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
	"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
	"GIT_TERMINAL_PROMPT=0",
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = testEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func upstream(t *testing.T) (dir, first, head string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "uploadpack.allowFilter", "true")
	write := func(name, data string, mode fs.FileMode) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "first\n", 0644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "first")
	first = git(t, dir, "rev-parse", "HEAD")
	write("README.md", "second\n", 0644)
	write("go.mod", "module example.com/snapshot\n\ngo 1.23.0\n", 0644)
	write("cmd/app/main.go", "package main\n\nfunc main() {}\n", 0644)
	write("empty", "", 0644)
	write("name with spaces", "spaces\n", 0644)
	write("run.sh", "#!/bin/sh\n", 0755)
	write(".gitignore", "*.tmp\n", 0644)
	if err := os.Symlink("README.md", filepath.Join(dir, "readme-link")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "second")
	head = git(t, dir, "rev-parse", "HEAD")
	return
}

func newCheckout(t *testing.T) (*Manager, *checkout, string, string) {
	t.Helper()
	up, first, head := upstream(t)
	remote := &url.URL{
		Scheme: "file",
		Path:   "/" + strings.TrimPrefix(filepath.ToSlash(up), "/"),
	}
	m := NewManager(t.TempDir(), map[string]Config{
		"example/repo": {
			RemoteURL: remote.String(),
		},
	})
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	co, err := m.checkout(context.Background(), "example/repo", head)
	if err != nil {
		t.Fatal(err)
	}
	return m, co, first, head
}

func TestManagerValidation(t *testing.T) {
	for _, name := range []string{"", "one", "a/b/c", "../b", "a/.."} {
		m := NewManager(t.TempDir(), map[string]Config{
			name: {
				RemoteURL: "x",
			},
		})
		if err := m.validate(); err == nil {
			t.Errorf("Validate accepted %q", name)
		}
	}
}

func TestCheckout(t *testing.T) {
	m, co, _, head := newCheckout(t)
	if got := co.commit; got != head {
		t.Fatalf("commit = %q; want %q", got, head)
	}
	if got := git(t, co.repo.dir, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("bare = %q", got)
	}
	if got := git(t, co.repo.dir, "config", "--get", "remote.origin.partialclonefilter"); got != "blob:none" {
		t.Errorf("filter = %q", got)
	}
	if got := git(t, co.repo.dir, "rev-parse", "refs/gomodfs/checkouts/"+head); got != head {
		t.Errorf("pin = %q", got)
	}
	blob := git(t, co.repo.dir, "rev-parse", head+":README.md")
	cmd := exec.Command("git", "cat-file", "-e", blob)
	cmd.Dir = co.repo.dir
	cmd.Env = append(testEnv, "GIT_NO_LAZY_FETCH=1")
	if err := cmd.Run(); err == nil {
		t.Errorf("blob %s was fetched eagerly", blob)
	}

	for path, want := range map[string]string{
		"README.md":       "second\n",
		"cmd/app/main.go": "package main\n\nfunc main() {}\n",
		".git/HEAD":       head + "\n",
		".git/shallow":    head + "\n",
		".git/config":     checkoutConfig,
	} {
		got, err := co.readFile(context.Background(), path)
		if err != nil || string(got) != want {
			t.Errorf("ReadFile(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	fi, err := co.stat(context.Background(), ".git/shallow")
	if err != nil || fi.Size() != 41 || fi.Mode() != 0444 {
		t.Errorf("shallow stat = %v, %v", fi, err)
	}
	fi, err = co.stat(context.Background(), "run.sh")
	if err != nil || fi.Mode()&0111 == 0 {
		t.Errorf("run.sh stat = %v, %v", fi, err)
	}
	fi, err = co.stat(context.Background(), "readme-link")
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("link stat = %v, %v", fi, err)
	}
	if got, err := co.readlink(context.Background(), "readme-link"); err != nil || got != "README.md" {
		t.Errorf("Readlink = %q, %v", got, err)
	}
	loose, err := co.readFile(context.Background(), ".git/objects/"+blob[:2]+"/"+blob[2:])
	if err != nil || len(loose) == 0 {
		t.Fatalf("current loose object: %v", err)
	}

	m2 := NewManager(m.root, m.repos)
	co2, err := m2.checkout(context.Background(), "example/repo", head)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := co2.readFile(context.Background(), "README.md"); string(got) != "second\n" {
		t.Errorf("reopened README = %q", got)
	}
}

func materializeCheckout(t *testing.T, co *checkout) string {
	t.Helper()
	dir := t.TempDir()
	var materialize func(string)
	materialize = func(p string) {
		ents, err := co.readDir(context.Background(), p)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", p, err)
		}
		for _, ent := range ents {
			rel := ent.name
			if p != "" {
				rel = p + "/" + ent.name
			}
			abs := filepath.Join(dir, filepath.FromSlash(rel))
			switch {
			case ent.mode.IsDir():
				if err := os.MkdirAll(abs, 0755); err != nil {
					t.Fatal(err)
				}
				materialize(rel)
			case ent.mode&fs.ModeSymlink != 0:
				target, err := co.readlink(context.Background(), rel)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, abs); err != nil {
					t.Fatal(err)
				}
			default:
				b, err := co.readFile(context.Background(), rel)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(abs, b, ent.mode.Perm()); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(abs, staticTime, staticTime); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	materialize("")
	// Loose object directories aren't enumerable. Copy only the selected
	// commit and its tree closure, never its parents or their objects.
	tree := git(t, co.repo.dir, "rev-parse", co.commit+"^{tree}")
	objects := []string{co.commit, tree}
	out := git(t, co.repo.dir, "ls-tree", "-r", "-t", "-z", co.commit)
	for _, line := range strings.Split(out, "\x00") {
		if line == "" {
			continue
		}
		fields := strings.Fields(strings.SplitN(line, "\t", 2)[0])
		if fields[1] != "commit" {
			objects = append(objects, fields[2])
		}
	}
	for _, sha := range objects {
		b, err := co.readFile(context.Background(), ".git/objects/"+sha[:2]+"/"+sha[2:])
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, ".git", "objects", sha[:2], sha[2:])
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0444); err != nil {
			t.Fatal(err)
		}
	}
	gitDir := filepath.Join(dir, ".git")
	if err := os.Chmod(gitDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(gitDir, 0755) })
	return dir
}

func TestCheckoutWorksWithGit(t *testing.T) {
	_, co, first, head := newCheckout(t)
	dir := materializeCheckout(t, co)
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", first[:2], first[2:])); !os.IsNotExist(err) {
		t.Fatalf("parent commit present: %v", err)
	}
	oldBlob := git(t, co.repo.dir, "rev-parse", first+":README.md")
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", oldBlob[:2], oldBlob[2:])); !os.IsNotExist(err) {
		t.Fatalf("historical blob present: %v", err)
	}
	index, err := co.indexFile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stamp := git(t, co.repo.dir, "log", "-1", "--format=%H:%ct", head)
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"diff-index", "--quiet", "HEAD"}, ""},
		{[]string{"diff", "--no-ext-diff", "--name-only", "--exit-code"}, ""},
		{[]string{"diff", "HEAD"}, ""},
		{[]string{"rev-parse", "HEAD"}, head},
		{[]string{"rev-parse", "--show-toplevel"}, filepath.ToSlash(dir)},
		{[]string{"rev-parse", "--is-shallow-repository"}, "true"},
		{[]string{"-c", "log.showsignature=false", "log", "-1", "--format=%H:%ct"}, stamp},
		{[]string{"rev-list", "--count", "HEAD"}, "1"},
		{[]string{"log", "--format=%H"}, head},
		{[]string{"show", "HEAD:README.md"}, "second"},
		{[]string{"grep", "-l", "-F", "func main"}, "cmd/app/main.go"},
		{[]string{"grep", "--cached", "-l", "-F", "func main"}, "cmd/app/main.go"},
		{[]string{"ls-files"}, git(t, co.repo.dir, "ls-tree", "-r", "--name-only", head)},
		{[]string{"ls-files", "--others", "--exclude-standard"}, ""},
		{[]string{"ls-tree", "-r", "--long", "-z", "HEAD"}, git(t, co.repo.dir, "ls-tree", "-r", "--long", "-z", head)},
		{[]string{"for-each-ref", "--format=%(refname)", "--merged=HEAD"}, ""},
		{[]string{"status", "--porcelain"}, ""},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			if got := git(t, dir, tt.args...); got != tt.want {
				t.Errorf("got %q; want %q", got, tt.want)
			}
		})
	}

	if after, err := os.ReadFile(filepath.Join(dir, ".git", "index")); err != nil || !bytes.Equal(index, after) {
		t.Errorf("Git changed the synthesized index: %v", err)
	}
}

func TestIndexKeepsBlobsLazy(t *testing.T) {
	_, co, _, _ := newCheckout(t)
	missing := func() string {
		return git(t, co.repo.dir, "rev-list", "--objects", "--missing=print", co.commit)
	}
	before := missing()
	if !strings.Contains(before, "\n?") {
		t.Fatal("fixture has no promised blobs")
	}
	ctx := context.Background()
	if _, err := co.readDir(ctx, ""); err != nil {
		t.Fatal(err)
	}
	idx, err := co.indexFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".git/index", "/.git/index"} {
		fi, err := co.stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != int64(len(idx)) || !fi.ModTime().Equal(staticTime) {
			t.Errorf("Stat(%q) = %v", p, fi)
		}
		data, err := co.readFile(ctx, p)
		if err != nil || !bytes.Equal(data, idx) {
			t.Errorf("ReadFile(%q) differs from Index: %v", p, err)
		}
	}
	if after := missing(); after != before {
		t.Error("directory/index access fetched promised blobs")
	}
	idx[0] = 0
	if again, err := co.indexFile(ctx); err != nil || string(again[:4]) != "DIRC" {
		t.Errorf("Index cache aliased returned bytes: %v", err)
	}
}

func TestIndexErrorRetry(t *testing.T) {
	_, co, _, _ := newCheckout(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := co.readFile(ctx, ".git/index"); err == nil {
		t.Error("ReadFile ignored index generation error")
	}
	if _, err := co.stat(ctx, ".git/index"); err == nil {
		t.Error("Stat ignored index generation error")
	}
	if co.index != nil {
		t.Error("failed Index populated cache")
	}
	if _, err := co.indexFile(t.Context()); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestConcurrentCheckouts(t *testing.T) {
	m, _, _, head := newCheckout(t)
	m = NewManager(m.root, m.repos)
	m.RegisterMetrics(prometheus.NewRegistry())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			co, err := m.checkout(context.Background(), "example/repo", head)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := co.readFile(context.Background(), "README.md"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
