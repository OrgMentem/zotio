// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// A keyless local read goes to /api/users/0, which serves whichever account the
// desktop is signed in to. The cached user ID (111) says nothing about that
// account, so a vault note recorded for users/111 must not take the text of a
// same-key note the desktop serves from users/222.
func TestFxdVaultRefusesNoteServedFromAnotherLibrary(t *testing.T) {
	yes := rootFlags{yes: true, maxChanges: -1, asJSON: true}
	for _, tc := range []struct {
		name   string
		region string
		args   []string
		newCmd func(*rootFlags) *cobra.Command
	}{
		// "local notes" is the pushed baseline, so without the check this is a
		// clean fast-forward that would replace the region.
		{name: "pull", region: "local notes", newCmd: newVaultPullCmd},
		{name: "resolve keep-remote", region: "edited notes", args: []string{vltCitekey, "--keep-remote"}, newCmd: newVaultResolveCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := &vltZotero{
				version: 6,
				html:    markdownToNoteHTML(vltCitekey, "another account's note"),
				parent:  vltItemKey,
				library: map[string]any{"type": "user", "id": 222},
			}
			vltServe(t, z, "111")
			outDir := t.TempDir()
			path, before := vltWriteNote(t, outDir, "users/111", tc.region)

			args := append(append([]string(nil), tc.args...), "--out", outDir)
			if _, err := vltRun(t, tc.newCmd, yes, args...); err == nil {
				t.Fatal("run succeeded, want a refusal for a note served from another library")
			}
			if _, _, _, writes, _ := z.snapshot(); writes != 0 {
				t.Fatalf("Zotero writes = %d, want none", writes)
			}
			vltAssertUnchanged(t, path, before)
		})
	}
}
