// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "time"

// FindingSource records where a finding's data came from and how fresh it is,
// so an agent can decide whether to trust it (local reads may be stale).
type FindingSource struct {
	Kind     string     `json:"kind"`
	SyncedAt *time.Time `json:"synced_at,omitempty"`
}

// RecommendedAction describes the preview-first next step for a diagnostic finding.
type RecommendedAction struct {
	Command string `json:"command,omitempty"`
	Text    string `json:"text,omitempty"`
}

// Finding is the stable diagnostic taxonomy (notes/roadmap.md). Identity is
// usually (kind, item_key); grouped findings carry detail in Evidence and may
// leave ItemKey empty when no single Zotero item owns the finding.
type Finding struct {
	Kind              string             `json:"kind"`
	Severity          string             `json:"severity"`
	ItemKey           string             `json:"item_key,omitempty"`
	Title             string             `json:"title,omitempty"`
	Evidence          map[string]any     `json:"evidence,omitempty"`
	Source            FindingSource      `json:"source"`
	Autofixable       bool               `json:"autofixable"`
	RecommendedAction *RecommendedAction `json:"recommended_action,omitempty"`
}

// FindingsReport is the common machine-readable diagnostic envelope.
type FindingsReport struct {
	Findings []Finding `json:"findings"`
}
