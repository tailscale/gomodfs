// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitrepo

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// catFile is a long-running "git cat-file --batch" process, which reads
// objects much faster than one process for each object. It is not safe for
// concurrent use.
type catFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// startCatFile starts a cat-file process for the repository at dir. The
// process is not stopped by a request context, because it serves later
// requests too.
func startCatFile(dir string) (*catFile, error) {
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Dir = dir
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFile{
		cmd: cmd,
		in:  in,
		out: bufio.NewReaderSize(out, 64<<10),
	}, nil
}

// read returns the contents of the object id. After an error, the process is
// in an unknown state and must be closed.
func (c *catFile) read(id string) ([]byte, error) {
	if _, err := fmt.Fprintf(c.in, "%s\n", id); err != nil {
		return nil, err
	}
	// The response is "<id> <type> <size>\n<contents>\n", or
	// "<id> missing\n".
	header, err := c.out.ReadString('\n')
	if err != nil {
		return nil, err
	}
	f := strings.Fields(header)
	if len(f) != 3 || f[0] != id {
		return nil, fmt.Errorf("git cat-file: unexpected response %q for %s", header, id)
	}
	size, err := strconv.Atoi(f[2])
	if err != nil {
		return nil, fmt.Errorf("git cat-file: unexpected response %q for %s", header, id)
	}
	b := make([]byte, size+1)
	if _, err := io.ReadFull(c.out, b); err != nil {
		return nil, err
	}
	return b[:size], nil
}

// close stops the process.
func (c *catFile) close() error {
	c.in.Close()
	return c.cmd.Wait()
}
