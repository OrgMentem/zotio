// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The Zotero local API rejects writes with distinctive bodies; classifyAPIError
// must turn those into read-only guidance, while leaving genuine auth errors alone.

package cli

import (
	"fmt"
	"strings"
	"testing"
)

func TestClassifyAPIErrorLocalWriteRejection(t *testing.T) {
	for _, msg := range []string{
		"POST /items returned HTTP 400: Endpoint does not support method",
		"PATCH /items/ABCD returned HTTP 501: Method not implemented",
	} {
		got := classifyAPIError(fmt.Errorf("%s", msg), &rootFlags{}).Error()
		if !strings.Contains(got, "read-only") || !strings.Contains(got, "ZOTERO_BASE_URL") {
			t.Errorf("%q -> expected read-only guidance, got: %s", msg, got)
		}
	}
}

func TestClassifyAPIErrorAuthNotMisclassified(t *testing.T) {
	// A genuine auth 400 (no local-API rejection strings) must not be relabeled.
	got := classifyAPIError(fmt.Errorf("POST /items returned HTTP 400: invalid key"), &rootFlags{}).Error()
	if strings.Contains(got, "read-only") {
		t.Errorf("auth 400 misclassified as a local read-only rejection: %s", got)
	}
}

func TestClassifyAPIErrorVersionConflict(t *testing.T) {
	got := classifyAPIError(fmt.Errorf("PATCH /items/A returned HTTP 412: Precondition Failed"), &rootFlags{}).Error()
	if !strings.Contains(got, "version conflict") || !strings.Contains(got, "sync") {
		t.Errorf("412 -> expected version-conflict/sync hint, got: %s", got)
	}
}
