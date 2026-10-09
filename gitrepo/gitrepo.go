// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package gitrepo serves immutable checkouts of Git commits over NFSv4.
//
// A long-lived [Manager] keeps one bare repository for each configured
// upstream and fetches commits when they are first checked out. [NewFS]
// serves a fixed set of checkouts, usually one set for each CI job.
package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// RepoName is an "owner/repo" name. A checkout of the repository is served at
// /repos/<owner>/<repo>.
type RepoName string

// Config describes a repository that the manager can check out.
type Config struct {
	// RemoteURL is the Git remote that commits are fetched from.
	RemoteURL string
}

// Manager owns the bare repositories and their shared caches. It is safe for
// concurrent use.
type Manager struct {
	repos   map[RepoName]*repository
	metrics *metrics
}

// metrics labels are bounded: commit hashes and paths are not labels.
type metrics struct {
	fetches       *prometheus.CounterVec
	fetchDuration *prometheus.HistogramVec
	blobReads     *prometheus.CounterVec
}

// NewManager returns a manager that keeps its bare repositories below root.
// Each repository is created when it is first checked out.
func NewManager(root string, repos map[RepoName]Config) (*Manager, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		repos: make(map[RepoName]*repository, len(repos)),
		metrics: &metrics{
			fetches: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "gomodfs_repo_fetch_total",
				Help: "repository commit checkouts by result (hit, fetched, error).",
			}, []string{"repo", "result"}),
			fetchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
				Name:    "gomodfs_repo_fetch_duration_seconds",
				Help:    "repository commit fetch duration.",
				Buckets: prometheus.DefBuckets,
			}, []string{"repo"}),
			blobReads: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "gomodfs_repo_blob_read_total",
				Help: "repository blob reads by result (hit, miss, error).",
			}, []string{"repo", "result"}),
		},
	}
	for name, cfg := range repos {
		if !validName(name) {
			return nil, fmt.Errorf("gitrepo: invalid repository name %q; want owner/repo", name)
		}
		if cfg.RemoteURL == "" {
			return nil, fmt.Errorf("gitrepo: repository %q has an empty remote URL", name)
		}
		m.repos[name] = newRepository(name, filepath.Join(root, filepath.FromSlash(string(name))+".git"), cfg.RemoteURL, m.metrics)
	}
	return m, nil
}

// RegisterMetrics registers the manager's metrics with reg.
func (m *Manager) RegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(m.metrics.fetches, m.metrics.fetchDuration, m.metrics.blobReads)
}

// Close stops the Git processes that the manager keeps running to read
// objects. After Close, the filesystems of the manager's checkouts can read
// only the files that are in the blob cache, and reads of other files fail.
// Close does not stop [Manager.Checkout], which can start new processes. Call
// Close after you stop all servers of the manager's checkouts.
func (m *Manager) Close() error {
	var errs []error
	for _, r := range m.repos {
		errs = append(errs, r.close())
	}
	return errors.Join(errs...)
}

// Checkout is an immutable commit that a [Manager] has fetched, with the
// names and modes of all of its files. Get one with [Manager.Checkout], and
// serve it with [NewFS]. One Checkout can be in many filesystems. A Checkout
// does not hold resources, so there is nothing to release.
type Checkout struct {
	repo    *repository
	commit  string
	tree    string                 // ID of the commit's root tree.
	dirs    map[string][]treeEntry // Tree ID to entries.
	objects *objects               // For .git.
}

// Repo returns the name of the checkout's repository.
func (c *Checkout) Repo() RepoName {
	return c.repo.name
}

// Commit returns the full SHA-1 ID of the checkout's commit.
func (c *Checkout) Commit() string {
	return c.commit
}

// Checkout returns a checkout of commit, a full SHA-1 commit ID, in repo. It
// fetches the commit from the remote if the bare repository does not contain
// it, then lists all trees of the commit with one git command: clients
// usually read most directories, and one command for each directory is much
// slower.
func (m *Manager) Checkout(ctx context.Context, repo RepoName, commit string) (*Checkout, error) {
	r := m.repos[repo]
	if r == nil {
		return nil, fmt.Errorf("gitrepo: unknown repository %q", repo)
	}
	if !validSHA1(commit) {
		return nil, fmt.Errorf("gitrepo: invalid full commit SHA %q", commit)
	}
	tree, err := r.fetch(ctx, commit)
	if err == nil {
		var entries []treeEntry
		if entries, err = r.readTree(ctx, tree); err == nil {
			return newCheckout(r, commit, tree, entries), nil
		}
	}
	return nil, fmt.Errorf("gitrepo: checking out %s at %s: %w", repo, commit, err)
}

// newCheckout returns a checkout of commit, whose root tree is tree. Entries
// are all trees and files of the commit, from [repository.readTree].
func newCheckout(r *repository, commit, tree string, entries []treeEntry) *Checkout {
	dirs := map[string][]treeEntry{
		tree: nil,
	}
	ids := map[string]string{ // Tree path to ID.
		".": tree,
	}
	for _, e := range entries {
		if e.mode&0o170000 == 0o040000 {
			ids[e.name] = e.id
			// A tree that is the same as an earlier tree gets the same
			// entries again.
			dirs[e.id] = nil
		}
		parent := ids[path.Dir(e.name)]
		e.name = path.Base(e.name)
		dirs[parent] = append(dirs[parent], e)
	}
	return &Checkout{
		repo:    r,
		commit:  commit,
		tree:    tree,
		dirs:    dirs,
		objects: newObjects(commit, tree, entries),
	}
}

// validName reports whether name has the form "owner/repo", with segments
// that are usable as file and NFS names.
func validName(name RepoName) bool {
	owner, repo, _ := strings.Cut(string(name), "/")
	validSegment := func(s string) bool {
		return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
	}
	return validSegment(owner) && validSegment(repo)
}

func validSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
