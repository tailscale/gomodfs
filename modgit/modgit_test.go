package modgit

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestModgit(t *testing.T) {
	gitDir := t.TempDir()

	var d Downloader
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

	var d Downloader
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

func TestObject(t *testing.T) {
	buf := make([]byte, 10002)
	wantBuf := make([]byte, 20)
	for size := 0; size <= len(buf); size++ {
		o := object{"blob", buf[:size]}
		got := o.encodedSize()
		wantHdr := fmt.Appendf(wantBuf[:0], "%s %d\x00", o.typ, size)
		want := len(wantHdr) + size
		if got != want {
			t.Errorf("object.encodedSize() = %d, want %d for size %d", got, want, size)
		}
	}
}
