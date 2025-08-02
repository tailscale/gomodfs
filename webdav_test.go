// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"testing"

	"github.com/tailscale/gomodfs/store"
)

func TestParseWDPath(t *testing.T) {
	tests := []struct {
		path string
		want gmPath
	}{
		{
			path: ".gomodfs-status",
			want: gmPath{WellKnown: ".gomodfs-status"},
		},
		{
			path: ".Spotlight-V100",
			want: gmPath{NotExist: true},
		},
		{path: "", want: gmPath{}},
		{path: "cache", want: gmPath{}},
		{path: "cache/", want: gmPath{}},
		{path: "cache/download", want: gmPath{}},
		{path: "cache/download/", want: gmPath{}},
		{
			path: "cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.ziphash",
			want: gmPath{
				CacheDownloadFileExt: "ziphash",
				ModVersion:           store.ModuleVersion{Module: "go4.org/mem", Version: "v0.0.0-20240501181205-ae6ca9944745"},
			},
		},
		{
			path: "cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.bogusext",
			want: gmPath{NotExist: true},
		},
		{
			path: "cache/download/github.com/pkg/errors/@v/",
			want: gmPath{},
		},
		{
			path: "cache/download/github.com/!azure!a!d/microsoft-authentication-library-for-go/@v/v1.2.1.ziphash",
			want: gmPath{
				ModVersion:           store.ModuleVersion{Module: "github.com/AzureAD/microsoft-authentication-library-for-go", Version: "v1.2.1"},
				CacheDownloadFileExt: "ziphash",
			},
		},
		{
			path: "cache/download/github.com/go-json-experiment/json/@v/v0.0.0-20250223041408-d3c622f1b874.partial",
			want: gmPath{
				NotExist: true,
			},
		},
		{
			path: "go4.org/mem@v0.0.0-20240501181205-ae6ca9944745/mem.go",
			want: gmPath{
				ModVersion: store.ModuleVersion{Module: "go4.org/mem", Version: "v0.0.0-20240501181205-ae6ca9944745"},
				InZip:      true,
				Path:       "mem.go",
			},
		},
		{
			path: "go4.org/mem@v0.0.0-20240501181205-ae6ca9944745",
			want: gmPath{
				ModVersion: store.ModuleVersion{Module: "go4.org/mem", Version: "v0.0.0-20240501181205-ae6ca9944745"},
				InZip:      true,
				Path:       "",
			},
		},
		{
			path: "github.com/!data!dog/zstd@v1.4.5/zstd.go",
			want: gmPath{
				ModVersion: store.ModuleVersion{Module: "github.com/DataDog/zstd", Version: "v1.4.5"},
				InZip:      true,
				Path:       "zstd.go",
			},
		},
		{path: "foo", want: gmPath{}},
		{path: "foo/bar", want: gmPath{}},
		{path: "foo/bar/baz", want: gmPath{}},
		{path: "foo/bar/baz", want: gmPath{}},
		{
			path: "github.com/tailscale/web-client-prebuilt@v0.0.0-20250124233751-d4cd19a26976/build/", // with trailing slash
			want: gmPath{
				ModVersion: store.ModuleVersion{Module: "github.com/tailscale/web-client-prebuilt", Version: "v0.0.0-20250124233751-d4cd19a26976"},
				InZip:      true,
				Path:       "build", // without trailing slash
			},
		},
		{
			path: "tsgo-linux-amd64/1cd3bf1a6eaf559aa8c00e749289559c884cef09.extracted",
			want: gmPath{
				ModVersion: store.ModuleVersion{
					Module:  "github.com/tailscale/go",
					Version: "tsgo-linux-amd64-1cd3bf1a6eaf559aa8c00e749289559c884cef09",
				},
				WellKnown: "tsgo.extracted",
			},
		},
		{
			path: "tsgo-linux-amd64/1cd3bf1a6eaf559aa8c00e749289559c884cef09/all.bash",
			want: gmPath{
				ModVersion: store.ModuleVersion{
					Module:  "github.com/tailscale/go",
					Version: "tsgo-linux-amd64-1cd3bf1a6eaf559aa8c00e749289559c884cef09",
				},
				InZip: true,
				Path:  "all.bash",
			},
		},
		{
			path: "tsgo-linux-amd64/1cd3bf1a6eaf559aa8c00e749289559c884cef09/bin/", // with trailing slash
			want: gmPath{
				ModVersion: store.ModuleVersion{
					Module:  "github.com/tailscale/go",
					Version: "tsgo-linux-amd64-1cd3bf1a6eaf559aa8c00e749289559c884cef09",
				},
				InZip: true,
				Path:  "bin", // without trailing slash
			},
		},
		{
			path: "tsgo-linux-amd64/1cd3bf1a6eaf559aa8c00e749289559c884cef09/bin/go",
			want: gmPath{
				ModVersion: store.ModuleVersion{
					Module:  "github.com/tailscale/go",
					Version: "tsgo-linux-amd64-1cd3bf1a6eaf559aa8c00e749289559c884cef09",
				},
				InZip: true,
				Path:  "bin/go",
			},
		},
		{
			path: "tsgo-darwin-arm64",
			want: gmPath{},
		},
	}

	for _, tt := range tests {
		got := parsePath(tt.path)
		if got != tt.want {
			t.Errorf("parseWDPath(%q) = %v; want %v", tt.path, got, tt.want)
		}
	}
}
