// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ci

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/tailscale/gomodfs"
)

var (
	runVerifyUsed      = flag.Bool("verify-used", false, "if set, runs TestVerifyUsed")
	runRelativeOpen    = flag.Bool("relative-open", false, "if set, runs TestRelativeOpen against the mounted $GOMODCACHE")
	runRepositoryMount = flag.Bool("repository-mount", false, "verify the CI NFS repository mount")
)

// TestRelativeOpen opens files in the mounted module cache by paths relative
// to a working directory on the mount, as cmd/asm does with #include files.
// On Windows, WinFsp used to pass such opens to gomodfs with the working
// directory's path upcased, because gomodfs's volume claimed to be
// case-insensitive and not case-preserving.
func TestRelativeOpen(t *testing.T) {
	if !*runRelativeOpen {
		t.Skip("only runs in CI with --relative-open set")
	}
	modCache := os.Getenv("GOMODCACHE")
	if modCache == "" {
		t.Fatal("GOMODCACHE not set")
	}
	t.Chdir(filepath.Join(modCache, "golang.org", "x", "sys@v0.16.0"))

	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("reading go.mod relative to the module dir: %v", err)
	}
	if !bytes.Contains(b, []byte("module golang.org/x/sys")) {
		t.Errorf("go.mod contents = %q; want the golang.org/x/sys module", b)
	}
	if _, err := os.Stat("no-such-file"); !os.IsNotExist(err) {
		t.Errorf("Stat of a missing relative path: err = %v; want not exist", err)
	}
}

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
	dir := os.Getenv("GOMODFS_REPO")
	if dir == "" {
		t.Fatal("GOMODFS_REPO must be the mounted fixture")
	}
	// The example server's fixed owner needn't match the CI runner.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_0", dir)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := git("status", "--porcelain"); got != "" {
		t.Errorf("dirty checkout: %s", got)
	}
	if got := git("log", "--format=%s"); got != "NFS repository fixture" {
		t.Errorf("git log = %q", got)
	}
	info, err := os.Stat(filepath.Join(dir, "run.sh"))
	if err != nil || info.Mode()&0111 == 0 {
		t.Errorf("mounted executable mode: %v, %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "main.link")); err != nil || target != "main.go" {
		t.Errorf("mounted symlink = %q, %v", target, err)
	}
	if f, err := os.OpenFile(filepath.Join(dir, "main.go"), os.O_WRONLY|os.O_APPEND, 0); err == nil {
		f.Close()
		t.Error("opened main.go for writing on read-only checkout")
	}

	binary := filepath.Join(t.TempDir(), "fixture.exe")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=true", "-o", binary, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	bi, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(bi.Settings, debug.BuildSetting{Key: "vcs.modified", Value: "false"}) {
		t.Errorf("build settings %v; want vcs.modified=false", bi.Settings)
	}
}
