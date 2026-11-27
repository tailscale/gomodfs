// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/tailscale/nfsv4"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// upstream returns a repository with a regular file, an executable, a
// symlink, a subdirectory, and a submodule.
func upstream(t *testing.T) (dir, commit string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-q", "--object-format=sha1")
	for name, content := range map[string]string{
		"hello.txt":   "hello\n",
		"run.sh":      "#!/bin/sh\n",
		"sub/file.go": "package sub\n",
		"dup/file.go": "package sub\n", // Same tree as sub.
	} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("hello.txt", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "update-index", "--chmod=+x", "run.sh")
	git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",module")
	git(t, dir, "commit", "-qm", "initial")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

func newManager(t *testing.T, url string) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), map[RepoName]Config{
		"example/repo": {
			RemoteURL: url,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return m
}

func walk(t *testing.T, f nfsv4.FS, p string) (nfsv4.FileHandle, *nfsv4.Attrs, error) {
	t.Helper()
	r := (&nfsv4.Request{}).WithContext(t.Context())
	fh, err := f.Root(r)
	if err != nil {
		t.Fatal(err)
	}
	for name := range strings.SplitSeq(p, "/") {
		if name == "" {
			continue
		}
		if fh, _, err = f.Lookup(r, fh, name); err != nil {
			return nil, nil, err
		}
	}
	attrs, err := f.GetAttr(r, fh, nfsv4.AttrMask{})
	return fh, attrs, err
}

func list(t *testing.T, f nfsv4.FS, p string) []string {
	t.Helper()
	fh, _, err := walk(t, f, p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	var names []string
	_, err = f.ReadDir((&nfsv4.Request{}).WithContext(t.Context()), fh, nfsv4.ReadDirArgs{}, func(e nfsv4.DirEntry) bool {
		names = append(names, e.Name)
		return true
	})
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", p, err)
	}
	return names
}

func read(t *testing.T, f nfsv4.FS, p string) string {
	t.Helper()
	fh, _, err := walk(t, f, p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	buf := make([]byte, 100)
	n, eof, err := f.Read((&nfsv4.Request{}).WithContext(t.Context()), fh, 0, buf)
	if err != nil || !eof {
		t.Fatalf("Read(%s) = eof %v, %v", p, eof, err)
	}
	return string(buf[:n])
}

func TestFS(t *testing.T) {
	dir, commit := upstream(t)
	m := newManager(t, dir)
	co, err := m.Checkout(t.Context(), "example/repo", commit)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFS(FSOptions{
		UID:       1000,
		GID:       1001,
		Checkouts: []*Checkout{co},
	})
	if err != nil {
		t.Fatal(err)
	}

	for p, want := range map[string][]string{
		"":                          {"repos"},
		"repos":                     {"example"},
		"repos/example/repo":        {"dup", "hello.txt", "link", "module", "run.sh", "sub", ".git"},
		"repos/example/repo/sub":    {"file.go"},
		"repos/example/repo/dup":    {"file.go"},
		"repos/example/repo/module": nil,
	} {
		if got := list(t, f, p); !slices.Equal(got, want) {
			t.Errorf("list(%q) = %q; want %q", p, got, want)
		}
	}
	if got := read(t, f, "repos/example/repo/sub/file.go"); got != "package sub\n" {
		t.Errorf("sub/file.go = %q", got)
	}
	fh, _, _ := walk(t, f, "repos/example/repo/link")
	if got, err := f.ReadLink((&nfsv4.Request{}).WithContext(t.Context()), fh); got != "hello.txt" || err != nil {
		t.Errorf("ReadLink = %q, %v", got, err)
	}
	for p, want := range map[string]nfsv4.Attrs{
		"repos/example/repo/hello.txt": {Type: nfsv4.TypeReg, Mode: 0o444, Size: 6},
		"repos/example/repo/run.sh":    {Type: nfsv4.TypeReg, Mode: 0o555, Size: 10},
		"repos/example/repo/link":      {Type: nfsv4.TypeSymlink, Mode: 0o777, Size: 9},
		"repos/example/repo/sub":       {Type: nfsv4.TypeDir, Mode: 0o555},
	} {
		_, got, err := walk(t, f, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got.Type != want.Type || got.Mode != want.Mode || got.Size != want.Size || got.UID != 1000 || got.GID != 1001 {
			t.Errorf("%s: got type %v mode %o size %d owner %d:%d; want type %v mode %o size %d owner 1000:1001",
				p, got.Type, got.Mode, got.Size, got.UID, got.GID, want.Type, want.Mode, want.Size)
		}
	}
	for _, p := range []string{"repos/other", "repos/example/other", "repos/example/repo/missing"} {
		if _, _, err := walk(t, f, p); !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, nfsv4.ErrNoEnt) {
			t.Errorf("%s: err = %v; want not exist", p, err)
		}
	}
}

func TestCheckoutFetchesNewCommits(t *testing.T) {
	dir, first := upstream(t)
	m := newManager(t, dir)
	if _, err := m.Checkout(t.Context(), "example/repo", first); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("second\n"), 0o644)
	git(t, dir, "commit", "-qam", "second")
	second := git(t, dir, "rev-parse", "HEAD")

	// Each view sees only its own commit.
	for commit, want := range map[string]string{first: "hello\n", second: "second\n"} {
		co, err := m.Checkout(t.Context(), "example/repo", commit)
		if err != nil {
			t.Fatal(err)
		}
		if co.Repo() != "example/repo" || co.Commit() != commit {
			t.Errorf("checkout of %s: Repo, Commit = %q, %q", commit, co.Repo(), co.Commit())
		}
		f, err := NewFS(FSOptions{
			Checkouts: []*Checkout{co},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, f, "repos/example/repo/hello.txt"); got != want {
			t.Errorf("%s: hello.txt = %q; want %q", commit, got, want)
		}
	}
}

// fetches returns the number of checkouts of example/repo with result.
func fetches(t *testing.T, m *Manager, result string) int {
	t.Helper()
	var d dto.Metric
	if err := m.metrics.fetches.WithLabelValues("example/repo", result).Write(&d); err != nil {
		t.Fatal(err)
	}
	return int(d.GetCounter().GetValue())
}

func TestCheckoutFetchesOnce(t *testing.T) {
	dir, commit := upstream(t)
	// The bare repository is an empty directory in the work tree of
	// upstream. Git must not use the upstream repository in its place.
	root := filepath.Join(dir, "cache")
	if err := os.MkdirAll(filepath.Join(root, "example", "repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, map[RepoName]Config{
		"example/repo": {
			RemoteURL: dir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for range 3 {
		if _, err := m.Checkout(t.Context(), "example/repo", commit); err != nil {
			t.Fatal(err)
		}
	}
	if got := fetches(t, m, "fetched"); got != 1 {
		t.Errorf("fetched = %d; want 1", got)
	}
	if got := fetches(t, m, "hit"); got != 2 {
		t.Errorf("hit = %d; want 2", got)
	}
	if _, err := os.Stat(filepath.Join(root, "example", "repo.git", "HEAD")); err != nil {
		t.Errorf("bare repository not created: %v", err)
	}
}

func TestErrors(t *testing.T) {
	dir, commit := upstream(t)
	m := newManager(t, dir)
	for name, err := range map[string]error{
		"unknown repo":   func() error { _, err := m.Checkout(t.Context(), "example/other", commit); return err }(),
		"short commit":   func() error { _, err := m.Checkout(t.Context(), "example/repo", commit[:7]); return err }(),
		"missing commit": func() error { _, err := m.Checkout(t.Context(), "example/repo", strings.Repeat("0", 40)); return err }(),
		"invalid name": func() error {
			_, err := NewManager(t.TempDir(), map[RepoName]Config{"repo": {RemoteURL: dir}})
			return err
		}(),
		"duplicate checkout": func() error {
			co, err := m.Checkout(t.Context(), "example/repo", commit)
			if err != nil {
				return nil
			}
			_, err = NewFS(FSOptions{
				Checkouts: []*Checkout{co, co},
			})
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s: got nil error", name)
		}
	}
}

// materialize copies the tree at p in f to dir.
func materialize(t *testing.T, f nfsv4.FS, p, dir string) {
	t.Helper()
	r := (&nfsv4.Request{}).WithContext(t.Context())
	fh, attrs, err := walk(t, f, p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	switch attrs.Type {
	case nfsv4.TypeDir:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range list(t, f, p) {
			materialize(t, f, p+"/"+name, filepath.Join(dir, name))
		}
	case nfsv4.TypeSymlink:
		target, err := f.ReadLink(r, fh)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
	default:
		buf := make([]byte, attrs.Size)
		if _, _, err := f.Read(r, fh, 0, buf); err != nil {
			t.Fatalf("Read(%s): %v", p, err)
		}
		if err := os.WriteFile(dir, buf, os.FileMode(attrs.Mode)|0o200); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDotGit(t *testing.T) {
	src, _ := upstream(t)
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module example.com/m\n\ngo 1.23\n"), 0o644)
	os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "-qm", "second")
	commit := git(t, src, "rev-parse", "HEAD")

	co, err := newManager(t, src).Checkout(t.Context(), "example/repo", commit)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFS(FSOptions{
		Checkouts: []*Checkout{co},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "repo")
	materialize(t, f, "repos/example/repo", dir)

	for _, tt := range []struct{ cmd, want string }{
		{"rev-parse HEAD", commit},
		{"status --porcelain", ""},
		{"log --format=%H:%s", commit + ":second"},
		{"cat-file -p HEAD:sub/file.go", "package sub"},
		{"grep -l package", "dup/file.go\nmain.go\nsub/file.go"},
		{"fsck --no-dangling", ""},
	} {
		if got := git(t, dir, strings.Fields(tt.cmd)...); got != tt.want {
			t.Errorf("git %s = %q; want %q", tt.cmd, got, tt.want)
		}
	}
	// The index tells Git that the checkout is clean, so git status needs
	// only the commit and its root tree: not the files, the subtrees, or
	// the blobs.
	dotGit := filepath.Join(t.TempDir(), "repo")
	materialize(t, f, "repos/example/repo/.git", filepath.Join(dotGit, ".git"))
	keep := []string{commit, git(t, src, "rev-parse", "HEAD^{tree}")}
	objs, _ := filepath.Glob(filepath.Join(dotGit, ".git", "objects", "*", "*"))
	for _, obj := range objs {
		if !slices.Contains(keep, filepath.Base(filepath.Dir(obj))+filepath.Base(obj)) {
			os.Remove(obj)
		}
	}
	if got := git(t, dotGit, "status", "--porcelain"); got != "" {
		t.Errorf("git status without files and subtrees = %q; want clean", got)
	}

	// Go builds with VCS stamping need git status and git log to work.
	build := exec.Command("go", "build", "-buildvcs=true", "-o", filepath.Join(t.TempDir(), "m"), ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

func TestClose(t *testing.T) {
	dir, commit := upstream(t)
	m := newManager(t, dir)
	co, err := m.Checkout(t.Context(), "example/repo", commit)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFS(FSOptions{
		Checkouts: []*Checkout{co},
	})
	if err != nil {
		t.Fatal(err)
	}
	read(t, f, "repos/example/repo/hello.txt") // Starts the cat-file process.
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// Read a file that is not in the blob cache.
	fh, _, err := walk(t, f, "repos/example/repo/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Read((&nfsv4.Request{}).WithContext(t.Context()), fh, 0, make([]byte, 100)); err == nil {
		t.Error("Read after Close succeeded")
	}
}
