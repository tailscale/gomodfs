// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"testing"

	"github.com/tailscale/gomodfs/store"
)

var pathTests = []struct {
	path string
	want gmPath
}{
	{
		path: statusFile,
		want: gmPath{WellKnown: statusFile},
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
	{
		// macOS client spam, trying to look for AppleDouble "sidecar" files
		// for legacy HFS+ metadata.
		path: "cache/download/github.com/prometheus/client_golang/@v/._v0.9.1.mod",
		want: gmPath{
			NotExist: true,
		},
	},
	{
		// same as above, but with "cache/download".
		path: "cache/download/go4.org/mem/@v/._v0.0.0-20240501181205-ae6ca9944745.mod",
		want: gmPath{
			NotExist: true,
		},
	},
	{
		// Don't ignore dot files in zips.
		path: "tailscale.com@v1.87.0-pre.0.20250814204648-fbb91758ac41/release/dist/qnap/files/Tailscale/shared/ui/.htaccess",
		want: gmPath{
			InZip: true,
			Path:  "release/dist/qnap/files/Tailscale/shared/ui/.htaccess",
			ModVersion: store.ModuleVersion{
				Module:  "tailscale.com",
				Version: "v1.87.0-pre.0.20250814204648-fbb91758ac41",
			},
		},
	},
}

func TestParsePath(t *testing.T) {
	for _, tt := range pathTests {
		got := parsePath(tt.path)
		if got != tt.want {
			t.Errorf("parsePath(%q) = %v; want %v", tt.path, got, tt.want)
		}
	}
}

func BenchmarkParsePath(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		for _, tt := range pathTests {
			if got := parsePath(tt.path); tt.want != got {
				b.Fatalf("parsePath(%q) = %v; want %v", tt.path, got, tt.want)
			}
		}
	}
}
