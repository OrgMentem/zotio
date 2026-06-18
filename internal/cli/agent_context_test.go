// PATCH(glean co0m): regression guard for agent-context discovery wiring.

package cli

import "testing"

func TestBuildAgentDiscoveryContext(t *testing.T) {
	t.Helper()
	d := buildAgentDiscoveryContext()
	if d == nil {
		t.Fatal("expected non-nil discovery")
	}
	if d.Source != "which" {
		t.Fatalf("source = %q, want which", d.Source)
	}
	if d.EntryCount != len(whichIndex) {
		t.Fatalf("entry_count = %d, want %d", d.EntryCount, len(whichIndex))
	}
	if len(d.CandidateCommands) != len(whichIndex) {
		t.Fatalf("candidate_commands len = %d, want %d", len(d.CandidateCommands), len(whichIndex))
	}
}
