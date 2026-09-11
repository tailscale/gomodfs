// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ci

import (
	"debug/buildinfo"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tailscale/gomodfs"
)

var (
	runVerifyUsed      = flag.Bool("verify-used", false, "if set, runs TestVerifyUsed")
	runRepositoryMount = flag.Bool("repository-mount", false, "verify the CI NFS repository mount")
)

func TestVerifyUsed(t *testing.T) {
	if !*runVerifyUsed {
		t.Skip("only runs in CI with --verify-used set")
	}

	var got gomodfs.StatusJSON
	res, err := http.Get("http://localhost:8080/status.json")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status code = %v; want 200", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding status.json: %v", err)
	}

	// Spot check a few expected ops to ensure stats are being recorded and that
	// the CI test actually exercised gomodfs and didn't just accidentally use
	// the default local disk path.
	for _, opName := range []string{
		"gitstore-PutModFile",
		"net-downloadZip-ext-info",
	} {
		os, ok := got.Ops[opName]
		if !ok {
			t.Errorf("missing %q in Ops", opName)
			continue
		}
		if os.NumSuccess() == 0 {
			t.Errorf("%s NumSuccess = 0; want > 0", opName)
		}
	}

	// TODO: gracefully shut down the server? meh. CI will clean up.
}

func TestRepositoryMount(t *testing.T) {
	if !*runRepositoryMount {
		t.Skip("only runs in CI with -repository-mount")
	}
	dir, commit := os.Getenv("GOMODFS_REPO"), os.Getenv("GOMODFS_REPO_COMMIT")
	if dir == "" || len(commit) != 40 {
		t.Fatal("GOMODFS_REPO and GOMODFS_REPO_COMMIT must identify the mounted fixture")
	}
	// The example server's fixed owner needn't match the CI runner.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.ToSlash(dir))
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOTOOLCHAIN", "local")
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %q: %v\n%s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Status can refresh the index and hide incorrect initial stat data.
	run("git", "diff-index", "--quiet", "HEAD")
	if got := run("git", "status", "--porcelain"); got != "" {
		t.Fatalf("dirty checkout: %s", got)
	}
	if got := run("git", "rev-parse", "HEAD"); got != commit {
		t.Fatalf("HEAD = %q; want %q", got, commit)
	}
	if got := run("git", "log", "-1", "--format=%ct"); got != "1704164645" {
		t.Fatalf("commit timestamp = %q", got)
	}
	if got := run("git", "grep", "-n", "nfs-repository-fixture", "--", "main.go"); !strings.Contains(got, "main.go:3:") {
		t.Fatalf("unexpected grep output: %q", got)
	}
	tree := run("git", "ls-tree", "HEAD")
	for _, name := range []string{"go.mod", "main.go", "run.sh"} {
		if !strings.Contains(tree, "\t"+name) {
			t.Errorf("ls-tree missing %s: %s", name, tree)
		}
	}
	if got := run("git", "ls-tree", "HEAD", "run.sh"); !strings.HasPrefix(got, "100755 blob ") {
		t.Errorf("run.sh is not executable: %s", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "run.sh"))
		if err != nil || info.Mode()&0111 == 0 {
			t.Fatalf("mounted executable mode: %v, %v", info, err)
		}
		if strings.Contains(tree, "\tmain.link") {
			if target, err := os.Readlink(filepath.Join(dir, "main.link")); err != nil || target != "main.go" {
				t.Fatalf("mounted symlink = %q, %v", target, err)
			}
		}
	}

	binary := filepath.Join(t.TempDir(), "fixture.exe")
	run("go", "build", "-buildvcs=true", "-o", binary, ".")
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for key, want := range map[string]string{
		"vcs":          "git",
		"vcs.revision": commit,
		"vcs.time":     "2024-01-02T03:04:05Z",
		"vcs.modified": "false",
	} {
		if got := settings[key]; got != want {
			t.Errorf("build setting %s = %q; want %q", key, got, want)
		}
	}
	for _, name := range []string{"main.go", ".git/index", ".git/index.lock"} {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err == nil {
			f.Close()
			t.Errorf("opened %s for writing on read-only checkout", name)
		}
	}
}
