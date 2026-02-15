// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package remotestore

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/tailscale/gomodfs/store"
	"golang.org/x/mod/module"
)

// ModVersionMap is a flat map of file paths to metadata, used as the JSON wire
// type for the modmap endpoint. Directories are implicit from the file paths.
type ModVersionMap map[string]FileMeta

// FileMeta contains the metadata for a single file in a module version.
type FileMeta struct {
	Size int64       `json:"size"`
	Mode fs.FileMode `json:"mode"`
}

const (
	apiPrefix    = "/api/v1/"
	modmapPrefix = apiPrefix + "modmap/"
	filePrefix   = apiPrefix + "file/"
	metaPrefix   = apiPrefix + "meta/"
)

// parseMVFromPath extracts a store.ModuleVersion from a URL path with the
// given prefix. It finds the "/@v/" separator, then unescapes the module path
// and version components.
//
// For modmap and file paths: prefix + escapedMod + "/@v/" + escapedVer
// For meta paths: prefix + escapedMod + "/@v/" + escapedVer + "." + ext
//
// Returns the ModuleVersion and, for meta paths, the file extension.
func parseMVFromPath(urlPath, prefix string) (store.ModuleVersion, string, error) {
	var zero store.ModuleVersion

	rest := strings.TrimPrefix(urlPath, prefix)
	if rest == urlPath {
		return zero, "", fmt.Errorf("path %q does not start with prefix %q", urlPath, prefix)
	}

	idx := strings.Index(rest, "/@v/")
	if idx < 0 {
		return zero, "", fmt.Errorf("path %q missing /@v/ separator", urlPath)
	}

	escapedMod := rest[:idx]
	afterAtV := rest[idx+len("/@v/"):]

	if escapedMod == "" || afterAtV == "" {
		return zero, "", fmt.Errorf("path %q has empty module or version", urlPath)
	}

	// For meta paths, split version from extension.
	var escapedVer, ext string
	if prefix == metaPrefix {
		// The extension is after the last dot: "v1.2.3.info" -> version "v1.2.3", ext "info"
		lastDot := strings.LastIndex(afterAtV, ".")
		if lastDot < 0 {
			return zero, "", fmt.Errorf("meta path %q missing file extension", urlPath)
		}
		escapedVer = afterAtV[:lastDot]
		ext = afterAtV[lastDot+1:]
		if ext != "info" && ext != "mod" && ext != "ziphash" {
			return zero, "", fmt.Errorf("unknown meta extension %q", ext)
		}
	} else {
		escapedVer = afterAtV
	}

	modPath, err := module.UnescapePath(escapedMod)
	if err != nil {
		return zero, "", fmt.Errorf("invalid escaped module %q: %w", escapedMod, err)
	}
	ver, err := module.UnescapeVersion(escapedVer)
	if err != nil {
		return zero, "", fmt.Errorf("invalid escaped version %q: %w", escapedVer, err)
	}

	return store.ModuleVersion{Module: modPath, Version: ver}, ext, nil
}

// mvToEscapedPath returns the escaped "escapedMod/@v/escapedVer" path fragment
// for a given ModuleVersion.
func mvToEscapedPath(mv store.ModuleVersion) (string, error) {
	escMod, err := module.EscapePath(mv.Module)
	if err != nil {
		return "", err
	}
	escVer, err := module.EscapeVersion(mv.Version)
	if err != nil {
		return "", err
	}
	return escMod + "/@v/" + escVer, nil
}
