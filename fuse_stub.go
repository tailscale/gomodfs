// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package gomodfs

import "errors"

func (f *FS) MountFUSE(mntPoint string, opt *MountOpts) (MountRunner, error) {
	return nil, errors.New("FUSE mounting is not supported on Windows; use NFS or WebDAV mode instead")
}
