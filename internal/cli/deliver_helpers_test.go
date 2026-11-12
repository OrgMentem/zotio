// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
)

func assertRedirectRefused(t *testing.T, status int, wantErr string, wantPath string, paths []string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("reported %d as success, want an error naming the status", status)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("error = %v, want it to contain %q", err, wantErr)
	}
	if len(paths) != 1 || paths[0] != wantPath {
		t.Fatalf("request paths = %v, want the refused redirect not to be followed", paths)
	}
}
