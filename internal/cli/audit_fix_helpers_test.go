// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
)

func assertMissingStoreIsPrecondition(t *testing.T, err error, out string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want precondition error, got nil (output %q)", out)
	}
	if got := ExitCode(err); got != 9 {
		t.Fatalf("exit code = %d, want 9 (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "zotio sync") {
		t.Fatalf("error %q lacks remediation", err)
	}
}
