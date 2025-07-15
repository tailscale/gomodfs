// The dumppack command is used to dump the contents of a pack file
// to a specified path for debugging purposes.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	objectpkg "github.com/go-git/go-git/v5/plumbing/object"
)

func main() {
	flag.Parse()
	file := flag.Arg(0)

	f, err := os.Open(file)
	if err != nil {
		log.Fatal(err)
	}
	if err := dumpPackfile(f); err != nil {
		log.Fatalf("Failed to dump packfile: %v", err)
	}
	log.Printf("done.")

}

func dumpPackfile(r io.Reader) error {
	sc := packfile.NewScanner(r)
	version, objects, err := sc.Header()
	if err != nil {
		return fmt.Errorf("failed to read packfile header: %w", err)
	}
	log.Printf("packfile version %d, %d objects", version, objects)
	n := 0
	for {
		oh, err := sc.NextObjectHeader()
		if err != nil {
			if err == io.EOF {
				log.Printf("end of packfile reached after %d/%d objects", n, objects)
				break // end of packfile
			}
			return fmt.Errorf("failed to read next object header: %w", err)
		}
		rc, err := sc.ReadObject()
		if err != nil {
			return fmt.Errorf("failed to read object %d: %w", n, err)
		}
		all, err := io.ReadAll(rc)
		if err != nil {
			return fmt.Errorf("failed to read object %d content: %w", n, err)
		}
		hash := sha1.Sum(all)

		log.Printf(" pack[%d]: ref=%02x, type=%s, size=%d, offset=%d", n, hash, oh.Type, oh.Length, oh.Offset)
		n++

		if oh.Type == plumbing.TreeObject {
			var t objectpkg.Tree
			if err := t.Decode(encodedObject{typ: oh.Type, b: all}); err != nil {
				log.Printf("  tree decode error: %v", err)
			} else {
				log.Printf("  tree %s, %d entries", t.Hash, len(t.Entries))
				for _, entry := range t.Entries {
					log.Printf("    %s %s", entry.Name, entry.Hash)
				}
			}

		}

		if uint32(n) == objects {
			break
		}
	}
	sum, err := sc.Checksum()
	if err != nil {
		return fmt.Errorf("failed to read packfile checksum: %w", err)
	}
	log.Printf("packfile checksum: %v", sum)
	return nil
}

type encodedObject struct {
	plumbing.EncodedObject
	typ plumbing.ObjectType
	b   []byte // binary contents of the object, after the "<type> <size>\x00" prefix
}

func (eo encodedObject) Type() plumbing.ObjectType { return eo.typ }

func (eo encodedObject) Hash() plumbing.Hash {
	if len(eo.b) < 1 {
		return plumbing.ZeroHash
	}
	h := sha1.Sum(eo.b)
	return plumbing.NewHash(hex.EncodeToString(h[:]))
}

func (eo encodedObject) Size() int64 {
	return int64(len(eo.b))
}

func (eo encodedObject) Reader() (io.ReadCloser, error) {
	//_, pay, _ := bytes.Cut(eo.b, []byte{'\x00'})
	return io.NopCloser(bytes.NewReader(eo.b)), nil
}
