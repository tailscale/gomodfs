package gitstore

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestModgit(t *testing.T) {
	gitDir := t.TempDir()

	var d Storage
	d.GitRepo = gitDir

	if err := d.git("init").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	const mod = "github.com/shurcooL/githubv4@v0.0.0-20240727222349-48295856cce7"

	ctx := context.Background()
	res, err := d.Get(ctx, mod)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !res.Downloaded {
		t.Fatalf("expected module to be downloaded, got %v", res)
	}
	t.Logf("Tree: %s", res.ModTree)

	out, err := d.git("cat-file", "-p", res.ModTree).Output()
	t.Logf("Got %v:\n%s", err, out)
}

func TestTreeBuilder(t *testing.T) {
	gitDir := t.TempDir()
	defer func() {
		if t.Failed() {
			t.Logf("Sleeping for 5m... to allow inspection of %s", gitDir)
			time.Sleep(5 * time.Minute)
		}
	}()

	var d Storage
	d.GitRepo = gitDir
	if err := d.git("init").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	for pass := range 2 {
		tb := newTreeBuilder(&d)

		for i := range 200 {
			if err := tb.addFile(
				fmt.Sprintf("a/b/c/d/file%d.txt", i),
				func() (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader(fmt.Sprintf("hello world %d\n", i))), nil
				},
				0644,
			); err != nil {
				t.Fatalf("addFile %d: %v", i, err)
			}
		}
		t.Logf("building tree...")
		treeHash, err := tb.buildTree("")
		if err != nil {
			t.Fatalf("addTree: %v", err)
		}
		const want = "66ee1f462ae592ba00fc845aa1d70d0f12e688fb"
		if treeHash != want {
			t.Fatalf("got treeHash %q, want %q", treeHash, want)
		}

		if st, err := tb.sendToGit(); err != nil {
			t.Fatalf("sendToGit: %v", err)
		} else {
			want := &sendToGitStats{}
			if pass == 0 {
				want.Trees = 5
				want.TreeBytes = 7802
				want.Blobs = 200
				want.BlobBytes = 3090
			}
			if !reflect.DeepEqual(st, want) {
				t.Errorf("pass[%d] sendToGit: got stats %+v, want %+v", pass, st, want)
			}
		}

		if pass == 0 {
			got, err := d.git("cat-file", "-p", treeHash).Output()
			if err != nil {
				t.Fatalf("git cat-file -p %s: %v", treeHash, err)
			}
			t.Logf("got: %s", got)
		}
	}
}
