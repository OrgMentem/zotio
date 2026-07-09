// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The reviewable-import manifest — the
// editable contract between `import resolve` (scan -> resolved proposals) and
// `import apply` (create items / attach files through the mutation engine).

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const importManifestSchemaVersion = 2

type importDiscovery struct {
	Direction   string   `json:"direction,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	CitedByKeys []string `json:"cited_by_keys,omitempty"`
	Count       int      `json:"count,omitempty"`
}

// importManifestEntry is one proposed import action a user can review and edit
// before applying.
type importManifestEntry struct {
	Path           string           `json:"path"`                      // absolute PDF path (for attach)
	Classification string           `json:"classification"`            // new|duplicate|attach_candidate|unidentified
	Action         string           `json:"action"`                    // create|attach|skip
	IdentifierType string           `json:"identifier_type,omitempty"` // e.g. doi
	Identifier     string           `json:"identifier,omitempty"`
	MatchedKey     string           `json:"matched_key,omitempty"` // existing item key for attach
	Title          string           `json:"title,omitempty"`
	Item           map[string]any   `json:"item,omitempty"` // resolved item to create
	Status         string           `json:"status"`         // resolved|unresolved
	Note           string           `json:"note,omitempty"`
	Discovery      *importDiscovery `json:"discovery,omitempty"`
}

// importManifest is the full reviewable plan.
type importManifest struct {
	SchemaVersion int                   `json:"schema_version"`
	Dir           string                `json:"dir,omitempty"`
	Entries       []importManifestEntry `json:"entries"`
}

// manifestActionForStatus maps a scan classification to its default action:
// new -> create, attach_candidate -> attach, unidentified -> recognize, duplicate -> skip.
func manifestActionForStatus(status string) string {
	switch status {
	case "new":
		return "create"
	case "attach_candidate":
		return "attach"
	case "unidentified":
		return "recognize"
	default:
		return "skip"
	}
}

// readImportManifest reads a manifest from a file path, or stdin when path is
// "-". It validates the schema version.
func readImportManifest(path string) (importManifest, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return importManifest{}, fmt.Errorf("opening manifest: %w", err)
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return importManifest{}, fmt.Errorf("reading manifest: %w", err)
	}
	var m importManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return importManifest{}, fmt.Errorf("parsing manifest: %w", err)
	}
	if m.SchemaVersion != 1 && m.SchemaVersion != importManifestSchemaVersion {
		return importManifest{}, fmt.Errorf("unsupported manifest schema_version %d (want 1 or %d)", m.SchemaVersion, importManifestSchemaVersion)
	}
	return m, nil
}

// writeImportManifest writes the manifest as indented JSON.
func writeImportManifest(w io.Writer, m importManifest) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}
