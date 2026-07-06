// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase2): cover cross-platform launch dispatch and verify-mode launch suppression.

package cli

import (
	"reflect"
	"testing"

	"zotio/internal/cliutil"
)

// PATCH(glean roadmap-phase2): verify OS dispatch stays pure and stable.
func TestLaunchCommand(t *testing.T) {
	uri := "zotero://select/library"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{goos: "darwin", name: "open", args: []string{uri}},
		{goos: "linux", name: "xdg-open", args: []string{uri}},
		{goos: "windows", name: "rundll32", args: []string{"url.dll,FileProtocolHandler", uri}},
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			name, args := launchCommand(tt.goos, uri)
			if name != tt.name {
				t.Fatalf("name = %q, want %q", name, tt.name)
			}
			if !reflect.DeepEqual(args, tt.args) {
				t.Fatalf("args = %#v, want %#v", args, tt.args)
			}
		})
	}
}

// PATCH(glean roadmap-phase2): verify-mode launch must not invoke desktop handlers.
func TestLaunchURIVerifyEnv(t *testing.T) {
	t.Setenv(cliutil.VerifyEnvVar, "1")

	if err := launchURI("zotero://select/library"); err != nil {
		t.Fatalf("launchURI returned error under verify env: %v", err)
	}
}
