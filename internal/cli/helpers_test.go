// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The read envelope's two extension points: sibling top-level keys (extra) and
// command-specific meta annotations (DataProvenance.MetaExtra). Both refuse a
// key the envelope already owns, because dropping such a key silently turned a
// caller's bug into missing output that no surface reported.

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeEnvelopeKeys returns the envelope's top-level keys undecoded, so a test
// can assert what the envelope carries without a struct that would quietly
// ignore an unexpected key.
func decodeEnvelopeKeys(t *testing.T, wrapped json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(wrapped, &envelope); err != nil {
		t.Fatalf("decoding envelope %s: %v", wrapped, err)
	}
	return envelope
}

// An extra key is a sibling of .results, never a member of it: the invariant
// every jq pipeline depends on is that .results holds exactly what the command
// matched.
func TestWrapWithProvenanceExtraAddsSiblingKeysBesideResults(t *testing.T) {
	wrapped, err := wrapWithProvenanceExtra(
		json.RawMessage(`[{"key":"ATTN"}]`),
		DataProvenance{Source: "local", ResourceType: "items"},
		map[string]any{"near_title_matches": []nearTitleMatch{{Key: "ATTN", Title: "Attention Is All You Need", Score: 0.8}}},
	)
	if err != nil {
		t.Fatalf("wrapWithProvenanceExtra: %v", err)
	}
	envelope := decodeEnvelopeKeys(t, wrapped)

	var results []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(envelope["results"], &results); err != nil {
		t.Fatalf("decoding results: %v", err)
	}
	if len(results) != 1 || results[0].Key != "ATTN" {
		t.Errorf("results = %s, want the one matched item", envelope["results"])
	}

	var near []nearTitleMatch
	if err := json.Unmarshal(envelope["near_title_matches"], &near); err != nil {
		t.Fatalf("decoding near_title_matches: %v", err)
	}
	if len(near) != 1 || near[0].Key != "ATTN" || near[0].Score != 0.8 {
		t.Errorf("near_title_matches = %s, want the advisory row with its score", envelope["near_title_matches"])
	}
}

// The guard used to `continue` past a reserved key: no error, no warning, and
// the data the caller asked to publish simply absent. A caller colliding on
// .results or .meta has a bug, so the bug is reported instead of being turned
// into missing output.
func TestWrapWithProvenanceExtraRefusesReservedKeys(t *testing.T) {
	for _, reserved := range []string{"results", "meta"} {
		t.Run(reserved, func(t *testing.T) {
			wrapped, err := wrapWithProvenanceExtra(
				json.RawMessage(`[{"key":"ATTN"}]`),
				DataProvenance{Source: "local"},
				map[string]any{reserved: []string{"clobbered"}},
			)
			if err == nil {
				t.Fatalf("extra[%q] was accepted, envelope = %s", reserved, wrapped)
			}
			if !strings.Contains(err.Error(), reserved) {
				t.Errorf("error = %q, want the colliding key %q named", err, reserved)
			}
			if wrapped != nil {
				t.Errorf("envelope = %s, want no output beside the error", wrapped)
			}
		})
	}
}

// Two reserved keys at once still name the same one on every run: an error a
// bug report cannot reproduce is worth less than one it can.
func TestWrapWithProvenanceExtraNamesAStableReservedKey(t *testing.T) {
	for range 8 {
		_, err := wrapWithProvenanceExtra(
			json.RawMessage(`[]`),
			DataProvenance{Source: "local"},
			map[string]any{"results": 1, "meta": 2},
		)
		if err == nil {
			t.Fatal("two reserved keys were accepted")
		}
		if !strings.Contains(err.Error(), `"meta"`) {
			t.Fatalf("error = %q, want the first reserved key (meta) named on every run", err)
		}
	}
}

// A command annotation belongs inside meta: it describes how the answer was
// produced, so it is not a sibling of the data.
func TestWrapWithProvenanceExtraMergesMetaAnnotations(t *testing.T) {
	wrapped, err := wrapWithProvenanceExtra(
		json.RawMessage(`[]`),
		DataProvenance{Source: "local", Reason: "local_only", MetaExtra: map[string]any{"title_lookup": "near_matches"}},
		nil,
	)
	if err != nil {
		t.Fatalf("wrapWithProvenanceExtra: %v", err)
	}
	var envelope struct {
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal(wrapped, &envelope); err != nil {
		t.Fatalf("decoding envelope %s: %v", wrapped, err)
	}
	if got := envelope.Meta["title_lookup"]; got != "near_matches" {
		t.Errorf("meta.title_lookup = %v, want near_matches (meta = %v)", got, envelope.Meta)
	}
	// The annotation is additive: provenance keys still read the same.
	if envelope.Meta["source"] != "local" || envelope.Meta["reason"] != "local_only" {
		t.Errorf("meta = %v, want the provenance keys unchanged", envelope.Meta)
	}
	// And it never becomes a top-level key.
	if _, leaked := decodeEnvelopeKeys(t, wrapped)["title_lookup"]; leaked {
		t.Error("meta annotation leaked to the envelope's top level")
	}
}

// Provenance owns where the data came from. A command annotation that collides
// with it would relabel the source of the answer, which is a caller bug for the
// same reason a reserved sibling key is.
func TestWrapWithProvenanceExtraRefusesMetaAnnotationsOverridingProvenance(t *testing.T) {
	wrapped, err := wrapWithProvenanceExtra(
		json.RawMessage(`[]`),
		DataProvenance{Source: "local", MetaExtra: map[string]any{"source": "live"}},
		nil,
	)
	if err == nil {
		t.Fatalf("MetaExtra overrode meta.source, envelope = %s", wrapped)
	}
	if !strings.Contains(err.Error(), "source") {
		t.Errorf("error = %q, want the colliding meta key named", err)
	}
	if wrapped != nil {
		t.Errorf("envelope = %s, want no output beside the error", wrapped)
	}
}
