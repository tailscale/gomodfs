// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ci

import (
	"encoding/json"
	"flag"
	"net/http"
	"testing"

	"github.com/tailscale/gomodfs"
)

var runVerifyUsed = flag.Bool("verify-used", false, "if set, runs TestVerifyUsed")

func TestVerifyUsed(t *testing.T) {
	if !*runVerifyUsed {
		t.Skip("only runs in CI with --verify-used set")
	}

	var got gomodfs.StatusJSON
	res, err := http.Get("http://localhost:8080/status.json")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status code = %v; want 200", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding status.json: %v", err)
	}

	// Spot check a few expected ops to ensure stats are being recorded and that
	// the CI test actually exercised gomodfs and didn't just accidentally use
	// the default local disk path.
	for _, opName := range []string{
		"gitstore-PutModFile",
		"net-downloadZip-ext-info",
	} {
		os, ok := got.Ops[opName]
		if !ok {
			t.Errorf("missing %q in Ops", opName)
			continue
		}
		if os.NumSuccess() == 0 {
			t.Errorf("%s NumSuccess = 0; want > 0", opName)
		}
	}

	// TODO: gracefully shut down the server? meh. CI will clean up.
}
