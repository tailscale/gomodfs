package gomodfs

import (
	"fmt"
	"maps"
	"path"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

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

	// isDotFile is whether the base file is for a dotfile such as
	// .DS_Store, .Spotlight-V100, ._., .hidden,
	// .metadata_never_index_unless_rootfs, ..metadata_never_index, etc
	// These may exist inside a zip, but they never exist elsewhere.
	isDotFile := name != "" && strings.HasPrefix(base, ".")

	if strings.HasPrefix(name, "tsgo-") {
		if isDotFile {
			// TODO(bradfitz): as far as I know, this is true (no useful dot
			// files in the goroot), and this is what we did in earlier versions
			// of gomodfs, so keep it for now.
			ret.NotExist = true
			return
		}
		return parseTSGoPath(name)
	}
	if dlSuffix, ok := strings.CutPrefix(name, "cache/download/"); ok {
		if isDotFile {
			ret.NotExist = true
			return
		}
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
		if isDotFile {
			ret.NotExist = true
			return
		}
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

func isHexChar(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
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

	hash, wantExtractedFile := strings.CutSuffix(firstSeg, ".extracted")
	if len(hash) != 40 {
		ret.NotExist = true
	}
	for _, b := range hash {
		if !isHexChar(b) {
			ret.NotExist = true
			return
		}
	}

	ret.ModVersion.Module = "github.com/tailscale/go"
	ret.ModVersion.Version = tsGoVersion(tsgoTriple{OS: goos, Arch: goarch, Hash: hash})
	if wantExtractedFile {
		ret.WellKnown = "tsgo.extracted"
		return
	}
	ret.Path = strings.TrimSuffix(rest, "/")
	ret.InZip = true
	return ret
}

type tsgoTriple struct {
	OS   string
	Arch string
	Hash string
}

var tsGoVersion = funcSmallSet(func(triple tsgoTriple) string {
	return fmt.Sprintf("tsgo-%s-%s-%s", triple.OS, triple.Arch, triple.Hash)
})

var tsGoZipRoot = funcSmallSet(func(triple tsgoTriple) string {
	return fmt.Sprintf("tsgo-%s-%s/%s", triple.OS, triple.Arch, triple.Hash)
})

type escPair struct {
	EscMod string
	EscVer string
}

// seePairs memoizes [parseModVersion].
// It grows unbounded, which in practice is fine.
var seenPairs sync.Map // escPair -> store.ModuleVersion

func parseModVersion(escMod, escVer string) (mv store.ModuleVersion, ok bool) {
	pair := escPair{EscMod: escMod, EscVer: escVer}
	if v, ok := seenPairs.Load(pair); ok {
		return v.(store.ModuleVersion), true
	}

	var err error
	mv.Module, err = module.UnescapePath(escMod)
	if err != nil {
		return mv, false
	}
	mv.Version, err = module.UnescapeVersion(escVer)
	if err != nil {
		return mv, false
	}

	seenPairs.Store(pair, mv)
	return mv, true
}

// funcSmallSet returns a memoized version of f.
//
// The implementation assumption is that the the set of possible K values is
// small. The implementation re-clones the prior map of all previous keys
// and values whenever a new key is seen.
//
// f should be a pure function (free of side effects and deterministic).
func funcSmallSet[K comparable, V any](f func(K) V) func(K) V {
	var latestMap atomic.Value
	var mu sync.Mutex

	// TODO(bradfitz): if Go 1.24's internal/sync.HashTrieMap is ever exported,
	// perhaps via a new generic version of sync.Map (Go 1.24's sync.Map is just
	// the any/any instantiation of internal/sync.HashTrieMap), then we should
	// use that here instead. But tailscale.com's syncs.Map (mutex + normal map) and
	// std sync.Map are both sad for concurrency + boxing-into-interface reasons,
	// respectively. So do a lame non-scalable version (that only works for small sets
	// of items) instead, naming the func appropriately to scare off users who might
	// use it for tons of items)

	return func(k K) V {
		latest, _ := latestMap.Load().(map[K]V)

		if v, ok := latest[k]; ok {
			return v // common case
		}

		mu.Lock()
		defer mu.Unlock()

		latest, _ = latestMap.Load().(map[K]V)
		if v, ok := latest[k]; ok {
			return v // lost fill race
		}

		v := f(k)

		clone := maps.Clone(latest)
		if clone == nil {
			clone = make(map[K]V)
		}
		clone[k] = v
		latestMap.Store(clone)

		return v
	}
}
