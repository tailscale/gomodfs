[![status: experimental](https://img.shields.io/badge/status-experimental-blue)](https://tailscale.com/kb/1167/release-stages/#experimental)

# gomodfs

`gomodfs` is a virtual filesystem (supporting FUSE, WebDAV, and NFS) that
implements a read-only filesystem emulating the [`GOMODCACHE` directory
layout](https://go.dev/ref/mod#module-cache) that has all Go modules accessible,
without making the `cmd/go` tool ever think it needs to download anything.
Instead, `gomodfs` itself downloads modules on demand to pretend that it was on
all disk to begin with.

The motivation of this project is to speed up CI build systems, by sharing the Go module cache
between builds (each build in its own ephemeral container or VM) and then
sharing the `gomodfs` filesystem into those containers or VMs (over virtiofs) as
a read-only filesystem. The guest builds (potentially running untrusted code
from malicious PRs) can usefully share a Go module cache that isn't writable.

Without `gomodfs`, the alternative is to put `GOMODCACHE` on a tmpfs and run
something like a local [Athens](https://github.com/gomods/athens) server. But
then each build ends up downloading tons of zip files at startup and extracting
potentially hundreds or gigabytes of dependencies to memory, adding considerable
overhead before the build even begins. Alternatively, you make all builds share
a writable disk, but then you can't run untrusted code.

# Frontends

`gomodfs` is accessible either via FUSE, WebDAV, or NFS.

FUSE is best for Linux. It's works on macOS too, but it's tedious,
especially on EC2 VMs where you need to configure SIP & MDM to get it
kernel extensions working.

`gomodfs` also includes a WebDAV and NFS server.

The WebDAV support came first and was found a little lacking (the
macOS kernel forces `noexec` on those mounts), so we also added NFS support.

Also, sharing either the WebDAV or FUSE mount over virtio-fs into Tart
VMs hits bugs and limitations in Apple's virtio-fs implementation.
See https://github.com/containers/podman/discussions/23886
and https://github.com/docker/for-mac/issues/7059.

As such, we ended up using NFS. `gomodfs` runs an NFS server on the host
and we mount it in the guest VMs.

# Backends

The gomodfs storage backend is abstract and can be implemented however you'd
like.

The primary implementation uses git, as most Go modules change very little
between releases, and git does content-addressable de-duping for free. The use
of git is somewhat abnormal: there are no `commit` objects. Only `tree` and
`blob` objects. And refs to those.

Future implementations of the storage interface might include:

* traditional GOMODCACHE on-disk layout
* S3/etc object storage

# Status

As of 2025-07-27, this is still all very new. Use with caution. It's starting to
work, but it might not. Bug reports welcome.
