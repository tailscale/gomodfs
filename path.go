package gomodfs

import (
	"fmt"
	"path"
	"reflect"
	"strings"

	"github.com/tailscale/gomodfs/store"
	"golang.org/x/mod/module"
)

// gmPath is the gomodfs-specific path structure
// for paths received from WebDAV Stat or OpenFile calls.
type gmPath struct {
	// WellKnown, if non-empty, indicates that the path is
	// for a well-known gomodfs path. The only possible value
	// so far is ".gomodfs-status", for the status file.
	WellKnown string

	// NotExist indicates that the path does not exist.
	// This is used for paths that we know operating systems
	// are just checking for, but don't exist in the gomodfs hierarchy.
	NotExist bool

	// CacheDownloadFileExt is one of "mod", "ziphash", "info"
	// if the path is for "cache/download/<module>/@v/<version>.<CacheDownloadFileExt>".
	// If non-empty, then ModVersion is also populated.
	CacheDownloadFileExt string

	// InZip, if true, means that the path is within the contents of a zip
	// file (i.e. not in "cache/download/" and deep enough in the filesystem
	// to have hit an a directory with an "@" sign, such as
	// "github.com/!azure/azure-sdk-for-go@v16.2.1+incompatible".
	// When true, ModVersion and Path are also populated.
	InZip bool

	// Path is the path within the zip file, if InZip is true.
	// For the root directory in the zip, Path is empty.
	Path string

	// ModVersion is the module version for the path, if yet known.
	// It's populated if CacheDownloadFileExt is non-empty or
	// InZip is true.
	ModVersion store.ModuleVersion
}

// String returns a debug representation of the wdPath for tests.
func (p gmPath) String() string {
	var sb strings.Builder
	sb.WriteByte('{')
	rv := reflect.ValueOf(p)
	for _, st := range reflect.VisibleFields(rv.Type()) {
		f := rv.FieldByIndex(st.Index)
		if f.IsZero() {
			continue
		}
		if sb.Len() > 1 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s:%v", st.Name, f.Interface())
	}
	sb.WriteByte('}')
	return sb.String()
}

// parsePath parses a path as received for a WebDAV Stat or OpenFile call
// and returns its godmodfs-specific structure.
func parsePath(name string) (ret gmPath) {
	if name == statusFile {
		ret.WellKnown = statusFile
		return
	}
	base := path.Base(name)
	if name != "" && isMetadataFile(base) {
		ret.NotExist = true
		return
	}
	if strings.HasPrefix(name, "tsgo-") {
		return parseTSGoPath(name)
	}
	if dlSuffix, ok := strings.CutPrefix(name, "cache/download/"); ok {
		escMod, escVer, ok := strings.Cut(dlSuffix, "/@v/")
		if !ok {
			// Not enough.
			return
		}
		ext := path.Ext(escVer)
		if ext == "" {
			return
		}
		switch ext {
		case ".mod", ".ziphash", ".info":
			ret.CacheDownloadFileExt = ext[1:]
		default:
			// Not a recognized cache/download file.
			ret.NotExist = true
			return
		}
		escVer = strings.TrimSuffix(escVer, ext)

		ret.ModVersion, ok = parseModVersion(escMod, escVer)
		if !ok {
			ret.NotExist = true
		}
		return
	}

	// Zip path.
	escMod, verAndPath, ok := strings.Cut(name, "@")
	if !ok {
		return
	}
	escVer, path, _ := strings.Cut(verAndPath, "/")

	ret.ModVersion, ok = parseModVersion(escMod, escVer)
	if ok {
		ret.Path = strings.TrimSuffix(path, "/")
		ret.InZip = true
	} else {
		ret.NotExist = true
	}

	return
}

// isMetadataFile returns true if the filename suggests that this is a metadata
// file that should not legitimately be part of any Go module and is being
// queried by mistake. The hardcoded list currently assumes running on MacOS.
func isMetadataFile(basename string) bool {
	if !strings.HasPrefix(basename, ".") {
		return false
	}

	// AppleDouble resource fork files
	if strings.HasPrefix(basename, "._") {
		return true
	}
	switch basename {
	case ".DS_Store",
		".Spotlight-V100",
		".fseventsd",
		".Trashes",
		".TemporaryItems",
		".VolumeIcon.icns",
		".localized",
		".metadata_never_index",
		".metadata_never_index_unless_rootfs",
		".DocumentRevisions-V100":
		return true
	}
	return false
}

func parseTSGoPath(name string) (ret gmPath) {
	name, ok := strings.CutPrefix(name, "tsgo-")
	if !ok {
		return
	}
	osHyphenArch, rest, ok := strings.Cut(name, "/")
	if !ok {
		return
	}
	goos, goarch, ok := strings.Cut(osHyphenArch, "-")
	if !ok {
		return
	}
	if !validTSGoOSARCH(goos, goarch) {
		ret.NotExist = true
		return
	}
	firstSeg, rest, _ := strings.Cut(rest, "/")
	m := tsgoRootLookupRx.FindStringSubmatch(firstSeg)
	if m == nil {
		ret.NotExist = true
		return
	}
	hash, wantExtractedFile := m[1], m[2] != ""

	ret.ModVersion.Module = "github.com/tailscale/go"
	ret.ModVersion.Version = fmt.Sprintf("tsgo-%s-%s-%s", goos, goarch, hash)
	if wantExtractedFile {
		ret.WellKnown = "tsgo.extracted"
		return
	}
	ret.Path = strings.TrimSuffix(rest, "/")
	ret.InZip = true
	return ret
}

func parseModVersion(escMod, escVer string) (mv store.ModuleVersion, ok bool) {
	var err error
	mv.Module, err = module.UnescapePath(escMod)
	if err != nil {
		return mv, false
	}
	mv.Version, err = module.UnescapeVersion(escVer)
	if err != nil {
		return mv, false
	}
	return mv, true
}
