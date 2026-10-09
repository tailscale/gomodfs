// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/gomodfs/internal/lru"
)

// blobCacheSize is the maximum number of blob bytes that each repository keeps
// in memory. Blobs are content-addressed, so all checkouts of a repository
// share the cache.
const blobCacheSize = 64 << 20

// repository is one bare Git repository.
type repository struct {
	name    RepoName
	dir     string
	url     string
	metrics *metrics

	fetchMu sync.Mutex // Serializes init and fetch.
	inited  bool

	blobsMu sync.Mutex
	blobs   lru.Cache[string, []byte]

	catMu  sync.Mutex // Serializes reads with cat.
	cat    *catFile   // nil until the first read, or after an error.
	closed bool       // Set by close; later reads fail.
}

func newRepository(name RepoName, dir, url string, m *metrics) *repository {
	r := &repository{
		name:    name,
		dir:     dir,
		url:     url,
		metrics: m,
	}
	r.blobs.MaxSize = blobCacheSize
	r.blobs.EntrySize = func(_ string, b []byte) int64 { return int64(len(b)) + 1 }
	return r
}

// command returns a git command in the repository. It sets GIT_DIR, so that
// Git does not look for a repository in the parent directories if r.dir is
// not a repository.
func (r *repository) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_DIR="+r.dir)
	return cmd
}

// fetch makes sure that the repository contains commit, and returns the ID
// of the commit's root tree.
func (r *repository) fetch(ctx context.Context, commit string) (string, error) {
	// Look for the commit before the lock, so that checkouts of commits
	// that the repository contains do not wait for fetches of other
	// commits. Before init, the repository can be missing, and the
	// command fails.
	if tree, err := r.rootTree(ctx, commit); err == nil {
		r.metrics.fetches.WithLabelValues(string(r.name), "hit").Inc()
		return tree, nil
	}
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	if !r.inited {
		// Init is safe to run on an existing repository. It fails if the
		// existing repository does not use SHA-1.
		cmd := exec.CommandContext(ctx, "git", "init", "--bare", "--quiet", "--object-format=sha1", r.dir)
		if err := run(cmd); err != nil {
			return "", err
		}
		r.inited = true
	}
	// Look again: the repository can already exist, or another fetch can
	// have added the commit while this one waited for the lock.
	if tree, err := r.rootTree(ctx, commit); err == nil {
		r.metrics.fetches.WithLabelValues(string(r.name), "hit").Inc()
		return tree, nil
	}
	start := time.Now()
	// The ref keeps the commit's objects in the repository and gives later
	// fetches a base to negotiate from.
	err := run(r.command(ctx, "fetch", "--quiet", "--no-tags", r.url, commit+":refs/gomodfs/"+commit))
	var tree string
	if err == nil {
		tree, err = r.rootTree(ctx, commit)
	}
	result := "fetched"
	if err != nil {
		result = "error"
	}
	r.metrics.fetches.WithLabelValues(string(r.name), result).Inc()
	r.metrics.fetchDuration.WithLabelValues(string(r.name)).Observe(time.Since(start).Seconds())
	return tree, err
}

func (r *repository) rootTree(ctx context.Context, commit string) (string, error) {
	out, err := r.command(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", commit+"^{commit}^{tree}").Output()
	if err != nil {
		return "", fmt.Errorf("commit not found: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// treeEntry is one entry of a Git tree object.
type treeEntry struct {
	name string
	mode uint32 // Git mode, such as 0o100644.
	id   string // Object ID.
	size int64  // Blob size; zero for trees and submodules.
}

// readTree lists the tree id and all of its subtrees. Names are paths from
// id. Each tree comes before its entries.
func (r *repository) readTree(ctx context.Context, id string) ([]treeEntry, error) {
	out, err := r.command(ctx, "ls-tree", "-r", "-t", "-l", "-z", id).Output()
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", id, err)
	}
	var entries []treeEntry
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// Each record is "<mode> <type> <id> <size>\t<name>".
		meta, name, ok := strings.Cut(string(rec), "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			return nil, fmt.Errorf("reading tree %s: malformed entry %q", id, rec)
		}
		mode, err := strconv.ParseUint(f[0], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("reading tree %s: malformed mode %q", id, f[0])
		}
		size, _ := strconv.ParseInt(f[3], 10, 64) // The size is "-" for trees and submodules.
		entries = append(entries, treeEntry{
			name: name,
			mode: uint32(mode),
			id:   f[2],
			size: size,
		})
	}
	return entries, nil
}

// readObject returns the contents of the object id.
func (r *repository) readObject(id string) ([]byte, error) {
	r.blobsMu.Lock()
	b, ok := r.blobs.GetOk(id)
	r.blobsMu.Unlock()
	if ok {
		r.metrics.blobReads.WithLabelValues(string(r.name), "hit").Inc()
		return b, nil
	}
	b, err := r.catFile(id)
	if err != nil {
		r.metrics.blobReads.WithLabelValues(string(r.name), "error").Inc()
		return nil, fmt.Errorf("reading object %s: %w", id, err)
	}
	r.metrics.blobReads.WithLabelValues(string(r.name), "miss").Inc()
	if len(b) < blobCacheSize {
		r.blobsMu.Lock()
		r.blobs.Set(id, b)
		r.blobsMu.Unlock()
	}
	return b, nil
}

// catFile reads the object id with the repository's cat-file process. It
// starts the process if necessary.
func (r *repository) catFile(id string) ([]byte, error) {
	r.catMu.Lock()
	defer r.catMu.Unlock()
	if r.closed {
		return nil, errors.New("gitrepo: manager is closed")
	}
	if r.cat == nil {
		cat, err := startCatFile(r.dir)
		if err != nil {
			return nil, err
		}
		r.cat = cat
	}
	b, err := r.cat.read(id)
	if err != nil {
		// The output stream is in an unknown state. Start a new process
		// for the next read.
		r.cat.close()
		r.cat = nil
	}
	return b, err
}

// close stops the repository's cat-file process. Later reads fail.
func (r *repository) close() error {
	r.catMu.Lock()
	defer r.catMu.Unlock()
	r.closed = true
	if r.cat == nil {
		return nil
	}
	err := r.cat.close()
	r.cat = nil
	return err
}

func run(cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", cmd.Args[1], err, bytes.TrimSpace(out))
	}
	return nil
}
