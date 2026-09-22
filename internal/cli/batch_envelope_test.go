// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
)

// The shared batch envelope gate must refuse a 2xx body that proves no
// outcome, accept a create response that repeats an index across success and
// successful, and refuse a truly conflicting claim. Each case fails if
// checkBatchEnvelope is reverted to the update-only strict rule or removed.
func TestBatchEnvelopeNonEnvelopeBodyIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"proxy error page": `{"error":"proxy error"}`,
		"empty object":     `{}`,
		"singleton":        `{"key":"K1","version":101}`,
		"truncated":        `{"successful":{"0":{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkBatchEnvelope([]byte(body), 1); err == nil {
				t.Fatalf("checkBatchEnvelope(%q) = nil, want refusal", body)
			}
		})
	}
}

func TestBatchEnvelopeAcceptsSuccessSuccessfulOverlap(t *testing.T) {
	body := `{"success":{"0":"NEWKEY11"},"successful":{"0":{"key":"NEWKEY11","version":1}},"unchanged":{},"failed":{}}`
	if err := checkBatchEnvelope([]byte(body), 1); err != nil {
		t.Fatalf("checkBatchEnvelope(overlap) = %v, want acceptance: success/successful repeat the same claim", err)
	}
}

func TestBatchEnvelopeRejectsConflictingClaim(t *testing.T) {
	body := `{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"bad"}}}`
	err := checkBatchEnvelope([]byte(body), 1)
	if err == nil {
		t.Fatal("checkBatchEnvelope(conflict) = nil, want refusal")
	}
	if !strings.Contains(err.Error(), `attributed index "0" twice`) {
		t.Fatalf("checkBatchEnvelope(conflict) = %q, want the double-claim detail", err)
	}
}

func TestBatchEnvelopeRejectsPartialCoverage(t *testing.T) {
	body := `{"successful":{"0":{"key":"K1"}},"success":{},"unchanged":{},"failed":{}}`
	err := checkBatchEnvelope([]byte(body), 2)
	if err == nil {
		t.Fatal("checkBatchEnvelope(partial) = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "attributed 1 of 2") {
		t.Fatalf("checkBatchEnvelope(partial) = %q, want the coverage detail", err)
	}
}
