// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// repository is one repository-scoped bare partial mirror.
type repository struct {
	name string
	dir  string
	cfg  Config
	mgr  *Manager // Back-reference for adding to metrics.

	fetchMu sync.Mutex
}

func (r *repository) command(ctx context.Context, args ...string) *exec.Cmd {
	all := []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	all = append(all, args...)
	cmd := exec.CommandContext(ctx, "git", all...)
	cmd.Dir = r.dir
	return cmd
}

func (r *repository) configure(ctx context.Context, add bool) error {
	verb := "set-url"
	if add {
		verb = "add"
	}
	if err := run(r.command(ctx, "remote", verb, "origin", r.cfg.RemoteURL), "configuring repository remote"); err != nil {
		if add {
			return err
		}
		// Repair a bare repository left without a remote by an interrupted or
		// older initialization.
		if err := run(r.command(ctx, "remote", "add", "origin", r.cfg.RemoteURL), "adding repository remote"); err != nil {
			return err
		}
	}
	commands := [][]string{
		{"config", "remote.origin.promisor", "true"},
		{"config", "remote.origin.partialclonefilter", "blob:none"},
		{"config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
	}
	for _, args := range commands {
		if err := run(r.command(ctx, args...), "configuring repository"); err != nil {
			return err
		}
	}
	return nil
}

func (r *repository) ensureCommit(ctx context.Context, commit string) (retErr error) {
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	if r.hasCommit(ctx, commit) {
		r.mgr.metrics.fetches.WithLabelValues(r.name, "hit").Inc()
		return r.pin(ctx, commit)
	}
	start := time.Now()
	defer func() {
		result := "fetched"
		if retErr != nil {
			result = "error"
		}
		r.mgr.metrics.fetches.WithLabelValues(r.name, result).Inc()
		r.mgr.metrics.fetchDuration.WithLabelValues(r.name).Observe(time.Since(start).Seconds())
	}()
	ref := "refs/gomodfs/checkouts/" + commit
	args := []string{"fetch", "--quiet", "--filter=blob:none", "--no-tags", "origin", commit + ":" + ref}
	if err := run(r.command(ctx, args...), "fetching commit"); err != nil {
		return err
	}
	if !r.hasCommit(ctx, commit) {
		return fmt.Errorf("fetched object %s is not the requested commit", commit)
	}
	return nil
}

func (r *repository) hasCommit(ctx context.Context, commit string) bool {
	out, err := r.command(ctx, "rev-parse", "--verify", "--end-of-options", commit+"^{commit}").Output()
	return err == nil && strings.TrimSpace(string(out)) == commit
}

func (r *repository) pin(ctx context.Context, commit string) error {
	return run(r.command(ctx, "update-ref", "refs/gomodfs/checkouts/"+commit, commit), "pinning commit")
}
