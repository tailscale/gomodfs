// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func createRepoFixture(dir string) (string, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2024-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2024-01-02T03:04:05Z")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %q: %w: %s", args, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main", "--object-format=sha1"},
		{"config", "user.name", "NFS CI"},
		{"config", "user.email", "nfs-ci@example.com"},
		{"config", "core.autocrlf", "false"},
	} {
		if _, err := git(args...); err != nil {
			return "", err
		}
	}
	for name, content := range map[string]string{
		"go.mod":  "module example.com/nfsfixture\n\ngo 1.23.0\n",
		"main.go": "package main\n\nfunc main() { println(\"nfs-repository-fixture\") }\n",
		"run.sh":  "#!/bin/sh\necho nfs-repository-fixture\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0755); err != nil {
		return "", err
	}
	// Windows runners may not grant the privilege required to create symlinks.
	_ = os.Symlink("main.go", filepath.Join(dir, "main.link"))
	for _, args := range [][]string{
		{"add", "."},
		{"update-index", "--chmod=+x", "run.sh"},
		{"-c", "commit.gpgsign=false", "commit", "-m", "NFS repository fixture"},
	} {
		if _, err := git(args...); err != nil {
			return "", err
		}
	}
	return git("rev-parse", "HEAD")
}
