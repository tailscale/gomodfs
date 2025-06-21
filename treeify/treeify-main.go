package main

import (
	"bytes"
	"crypto/sha1"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/tailscale/gomodfs/modgit"
)

var (
	gitDir = flag.String("git-dir", "/tmp/modgit", "the git directory to use")
)

func main() {
	flag.Parse()
	if flag.NArg() != 1 {
		log.SetFlags(0)
		log.Fatal("Usage: treeify [--git-dir=/path] <path-to-treeify>\n")
	}

	w := &TreeWriter{
		Downloader: &modgit.Downloader{
			GitRepo: *gitDir,
		},
	}

	if err := w.Downloader.CheckExists(); err != nil {
		log.SetFlags(0)
		log.Fatal(err)
	}

	path := flag.Arg(0)

	hash, err := w.addTree(path)
	if err != nil {
		log.SetFlags(0)
		log.Fatalf("Failed to add tree: %v", err)
	}
	fmt.Println(hash)
}

type hashArray = [sha1.Size]byte

type TreeWriter struct {
	Downloader *modgit.Downloader
	BlobInGit  map[hashArray]string // straight sha1 => git sha1

	dupBlobs     int
	newBlobs     int
	treesStarted int
	treesEnded   int
}

const execBits = 0111

func (w *TreeWriter) addTree(path string) (hash string, err error) {
	w.treesStarted++
	ents, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}

	var txtTree bytes.Buffer // of text tree

	for _, ent := range ents {
		// <mode> <filename>\0<binary object id>
		typ := "blob"
		mode := "100644"

		base := ent.Name()
		if base == ".git" || base == "node_modules" {
			continue
		}

		full := filepath.Join(path, base)
		fi, err := ent.Info()
		if err != nil {
			return "", fmt.Errorf("failed to get info for %s: %w", full, err)
		}
		if ent.IsDir() {
			mode = "040000"
			typ = "tree"
		} else {
			if fi.Mode()&execBits != 0 {
				mode = "100755"
			}
		}
		var entHash string
		if typ == "tree" {
			var err error
			entHash, err = w.addTree(full)
			if err != nil {
				return "", fmt.Errorf("failed to build sub-tree for %q: %w", full, err)
			}
			log.Printf("%d trees deep, %d dup blobs, %d new blobs, %d trees done; just did %s",
				w.treesStarted-w.treesEnded,
				w.dupBlobs,
				w.newBlobs,
				w.treesEnded,
				full)
		} else {
			all, err := os.ReadFile(full)
			if err != nil {
				return "", err
			}
			hash := sha1.Sum(all)
			var ok bool
			entHash, ok = w.BlobInGit[hash]
			if !ok {
				entHash, err = w.Downloader.AddBlob(bytes.NewReader(all))
				if err != nil {
					return "", fmt.Errorf("failed to add blob for %q: %w", full, err)
				}
				w.newBlobs++
				if w.BlobInGit == nil {
					w.BlobInGit = make(map[hashArray]string)
				}
				w.BlobInGit[hash] = entHash
			} else {
				w.dupBlobs++
			}
		}
		fmt.Fprintf(&txtTree, "%s %s %s\t%s\n", mode, typ, entHash, base)
	}

	w.treesEnded++
	return w.Downloader.AddTreeFromTextFormat(txtTree.Bytes())
}
