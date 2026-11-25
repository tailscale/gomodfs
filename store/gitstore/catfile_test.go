// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gitstore

import (
	"context"
	"errors"
	"testing"

	"github.com/tailscale/gomodfs/stats"
)

func TestGetObjectMissingEndsSpan(t *testing.T) {
	d := &Storage{
		GitRepo: t.TempDir(),
		Stats:   &stats.Stats{},
	}
	if err := d.git("init").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Cleanup(func() { d.StopGitHelperProcess() })

	const op = "gitstore.cat-file-batch.request"
	for range 3 {
		_, err := d.getObject(context.Background(), "0123456789012345678901234567890123456789", anyType)
		if !errors.Is(err, errMissing) {
			t.Fatalf("getObject error = %v; want errMissing", err)
		}
	}
	got := d.Stats.Clone()[op]
	if got == nil {
		t.Fatalf("no %q stats", op)
	}
	if got.Started != 3 || got.Ended != 3 || got.Errs != 0 {
		t.Errorf("%q stats = Started %d, Ended %d, Errs %d; want 3, 3, 0", op, got.Started, got.Ended, got.Errs)
	}
}
