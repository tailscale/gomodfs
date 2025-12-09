// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !windows

package gomodfs

import (
	"errors"
)

func (mfs *FS) MountWinFSP(mntDir string) (MountRunner, error) {
	return nil, errors.New("WinFSP mounting is only supported on Windows")
}
