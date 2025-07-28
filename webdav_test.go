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
		want wdPath
	}{
		{
			path: ".gomodfs-status",
			want: wdPath{WellKnown: ".gomodfs-status"},
		},
		{
			path: ".Spotlight-V100",
			want: wdPath{NotExist: true},
		},
		{path: "", want: wdPath{}},
		{path: "cache", want: wdPath{}},
		{path: "cache/", want: wdPath{}},
		{path: "cache/download", want: wdPath{}},
		{path: "cache/download/", want: wdPath{}},
		{
			path: "cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.ziphash",
			want: wdPath{
				CacheDownloadFileExt: "ziphash",
				ModVersion:           store.ModuleVersion{Module: "go4.org/mem", Version: "v0.0.0-20240501181205-ae6ca9944745"},
			},
		},
		{
			path: "cache/download/go4.org/mem/@v/v0.0.0-20240501181205-ae6ca9944745.bogusext",
			want: wdPath{NotExist: true},
		},
		{
			path: "cache/download/github.com/pkg/errors/@v/",
			want: wdPath{},
		},
		{
			path: "cache/download/github.com/!azure!a!d/microsoft-authentication-library-for-go/@v/v1.2.1.ziphash",
			want: wdPath{
				ModVersion:           store.ModuleVersion{Module: "github.com/AzureAD/microsoft-authentication-library-for-go", Version: "v1.2.1"},
				CacheDownloadFileExt: "ziphash",
			},
		},
		{
			path: "cache/download/github.com/go-json-experiment/json/@v/v0.0.0-20250223041408-d3c622f1b874.partial",
			want: wdPath{
				NotExist: true,
			},
		},
		{
			path: "go4.org/mem@v0.0.0-20240501181205-ae6ca9944745/mem.go",
			want: wdPath{
				ModVersion: store.ModuleVersion{Module: "go4.org/mem", Version: "v0.0.0-20240501181205-ae6ca9944745"},
				InZip:      true,
				Path:       "mem.go",
			},
		},
		{
			path: "go4.org/mem@v0.0.0-20240501181205-ae6ca9944745",
			want: wdPath{
				ModVersion: store.ModuleVersion{Module: "go4.org/mem", Version: "v0.0.0-20240501181205-ae6ca9944745"},
				InZip:      true,
				Path:       "",
			},
		},
		{
			path: "github.com/!data!dog/zstd@v1.4.5/zstd.go",
			want: wdPath{
				ModVersion: store.ModuleVersion{Module: "github.com/DataDog/zstd", Version: "v1.4.5"},
				InZip:      true,
				Path:       "zstd.go",
			},
		},
		{path: "foo", want: wdPath{}},
		{path: "foo/bar", want: wdPath{}},
		{path: "foo/bar/baz", want: wdPath{}},
		{path: "foo/bar/baz", want: wdPath{}},
		{
			path: "github.com/tailscale/web-client-prebuilt@v0.0.0-20250124233751-d4cd19a26976/build/", // with trailing slash
			want: wdPath{
				ModVersion: store.ModuleVersion{Module: "github.com/tailscale/web-client-prebuilt", Version: "v0.0.0-20250124233751-d4cd19a26976"},
				InZip:      true,
				Path:       "build", // without trailing slash
			},
		},
	}

	for _, tt := range tests {
		got := parseWDPath(tt.path)
		if got != tt.want {
			t.Errorf("parseWDPath(%q) = %v; want %v", tt.path, got, tt.want)
		}
	}
}
