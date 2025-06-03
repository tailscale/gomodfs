package modgit

import (
	"context"
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
	t.Logf("Tree: %s", res.Tree)

	out, err := d.git("cat-file", "-p", res.Tree).Output()
	t.Logf("Got %v:\n%s", err, out)
}
