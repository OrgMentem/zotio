package cli

import "testing"

// TestFxaLibraryHealthRejectsNegativeRequireFresh pins that a negative
// --require-fresh is a usage error. Before, it disabled the freshness check and
// also skipped the unsynced-library refusal, so a gated run over a store whose
// items never synced exited 0.
func TestFxaLibraryHealthRejectsNegativeRequireFresh(t *testing.T) {
	root, _, _, _ := newPreflightTestRoot(t)
	hsSeedCollectionsOnlyStore(t)
	root.SetArgs([]string{"--json", "library", "health", "--fail-on", "high", "--require-fresh=-1s"})
	err := root.Execute()
	if code := ExitCode(err); code != 2 {
		t.Fatalf("exit = %d (%v), want 2 (usage error)", code, err)
	}
}
