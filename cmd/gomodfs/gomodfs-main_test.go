// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import "testing"

func TestRepositoryName(t *testing.T) {
	for _, tt := range []struct {
		remote, want string
	}{
		{"https://github.com/tailscale/gomodfs", "tailscale/gomodfs"},
		{"https://github.com/tailscale/gomodfs.git", "tailscale/gomodfs"},
		{"https://github.com/tailscale/gomodfs/", "tailscale/gomodfs"},
		{"ssh://git@github.com/tailscale/gomodfs.git", "tailscale/gomodfs"},
		{"file:///tmp/fixture/example/repo", "example/repo"},
		{"git@github.com:tailscale/gomodfs.git", "tailscale/gomodfs"},
		{"github.com:tailscale/gomodfs", "tailscale/gomodfs"},
		{"host:/srv/git/tailscale/gomodfs.git", "tailscale/gomodfs"},
		{"/srv/git/tailscale/gomodfs.git", "tailscale/gomodfs"},
		{"./tailscale/gomodfs:x", "tailscale/gomodfs:x"}, // A slash before the colon: a path.
		{"https://github.com/gomodfs", ""},
		{"git@github.com:gomodfs.git", ""},
		{"https://github.com/%zz/repo", ""},
	} {
		got, err := repositoryName(tt.remote)
		if tt.want == "" {
			if err == nil {
				t.Errorf("repositoryName(%q) = %q; want error", tt.remote, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("repositoryName(%q) = %q, %v; want %q", tt.remote, got, err, tt.want)
		}
	}
}
