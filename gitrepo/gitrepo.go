// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package gitrepo provides immutable filesystem views of commits stored in
// repository-scoped bare, blobless Git mirrors.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Config describes a repository that the manager may serve.
type Config struct {
	// RemoteURL is the Git remote used to populate the local bare mirror.
	RemoteURL string
}

// Manager owns one bare repository for each configured upstream.
type Manager struct {
	root  string
	repos map[string]Config

	mu      sync.Mutex
	open    map[string]*repository
	metrics *metrics
}

// metrics contains repository operations with bounded labels. Commit hashes
// and paths are deliberately not labels.
type metrics struct {
	fetches       *prometheus.CounterVec
	fetchDuration *prometheus.HistogramVec
	objectReads   *prometheus.CounterVec
}

// NewManager returns a manager backed by root for the configured repositories.
func NewManager(root string, repos map[string]Config) *Manager {
	return &Manager{
		root:  root,
		repos: repos,
		metrics: &metrics{
			fetches: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "gomodfs_repo_fetch_total",
				Help: "repository commit fetches by result.",
			}, []string{"repo", "result"}),
			fetchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
				Name:    "gomodfs_repo_fetch_duration_seconds",
				Help:    "repository commit fetch duration.",
				Buckets: prometheus.DefBuckets,
			}, []string{"repo"}),
			objectReads: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "gomodfs_repo_object_read_total",
				Help: "repository object reads by result.",
			}, []string{"repo", "result"}),
		},
	}
}

// RegisterMetrics registers metrics for this manager.
func (m *Manager) RegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(m.metrics.fetches, m.metrics.fetchDuration, m.metrics.objectReads)
}

// repository opens or creates the local bare mirror for name.
func (m *Manager) repository(ctx context.Context, name string) (*repository, error) {
	cfg, ok := m.repos[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	if !validName(name) {
		return nil, fmt.Errorf("invalid repository name %q", name)
	}
	// Concurrent initialization also rewrites config in an existing mirror.
	// Serialize openers so they don't contend for Git's config lock.
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.open[name]; r != nil {
		return r, nil
	}

	dir := filepath.Join(m.root, filepath.FromSlash(name)+".git")
	if err := m.initializeRepoLocked(ctx, name, dir, cfg); err != nil {
		return nil, err
	}
	if m.open == nil {
		m.open = map[string]*repository{}
	}
	m.open[name] = &repository{
		name: name,
		dir:  dir,
		cfg:  cfg,
		mgr:  m,
	}
	return m.open[name], nil
}

// checkout returns an immutable filesystem view of commit in name.
func (m *Manager) checkout(ctx context.Context, name, commit string) (*checkout, error) {
	if !validSHA1(commit) {
		return nil, fmt.Errorf("invalid full commit SHA %q", commit)
	}
	r, err := m.repository(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := r.ensureCommit(ctx, commit); err != nil {
		return nil, err
	}
	return &checkout{
		repo:     r,
		commit:   commit,
		treeEnts: map[string][]dirent{},
	}, nil
}

// initializeRepoLocked initializes the repository at dir if it does not already
// exist. m.mu must be held.
func (m *Manager) initializeRepoLocked(ctx context.Context, name, dir string, cfg Config) error {
	r := &repository{
		name: name,
		dir:  dir,
		cfg:  cfg,
	}
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("repository path %q is not a directory", dir)
		}
		out, err := r.command(ctx, "rev-parse", "--is-bare-repository").Output()
		if err != nil || strings.TrimSpace(string(out)) != "true" {
			return fmt.Errorf("repository path %q is not a bare Git repository", dir)
		}
		return r.configure(ctx, false)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".repo-init-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	r.dir = tmp

	if err := run(r.command(ctx, "init", "--bare", "--quiet"), "initializing bare repository"); err != nil {
		return err
	}
	if err := r.configure(ctx, true); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	return nil
}

// validate reports whether the manager has a usable root and repository configuration.
func (m *Manager) validate() error {
	if m.root == "" {
		return errors.New("gitrepo: empty cache root")
	}
	for name, cfg := range m.repos {
		if !validName(name) {
			return fmt.Errorf("gitrepo: invalid repository name %q; want owner/repo", name)
		}
		if cfg.RemoteURL == "" {
			return fmt.Errorf("gitrepo: repository %q has an empty remote URL", name)
		}
	}
	return nil
}

// validName reports whether name is a two-segment repository export name,
// such as "owner/repo".
func validName(name string) bool {
	a, b, ok := strings.Cut(name, "/")
	if !ok || a == "" || b == "" || strings.Contains(b, "/") {
		return false
	}
	validSegment := func(s string) bool {
		if s == "." || s == ".." || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
			return false
		}
		for _, c := range s {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
				continue
			}
			return false
		}
		return true
	}
	return validSegment(a) && validSegment(b)
}

func run(cmd *exec.Cmd, what string) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", what, err, bytes.TrimSpace(out))
	}
	return nil
}
