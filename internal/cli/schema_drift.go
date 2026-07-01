// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Hand-written `schema drift` probe (not in the generated CLI). Captures a
// baseline fingerprint of the running Zotero's item-type/field schema and diffs a
// later live fetch against it, so a Zotero upgrade's new (or removed) item types,
// fields, and creator fields are surfaced. The schema is global to a Zotero install,
// not per-library, so the baseline is NOT group-scoped.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

// schemaSnapshot is an order-independent fingerprint of a Zotero install's
// item-type/field schema. Every slice is sorted and per-type maps are keyed by
// item type, so two snapshots of the same schema compare equal regardless of API
// response ordering — drift output is therefore deterministic. TypeFields and
// TypeCreators are populated only under --deep.
type schemaSnapshot struct {
	SchemaVersion string              `json:"schema_version,omitempty"`
	ItemTypes     []string            `json:"item_types"`
	ItemFields    []string            `json:"item_fields"`
	CreatorFields []string            `json:"creator_fields"`
	TypeFields    map[string][]string `json:"type_fields,omitempty"`
	TypeCreators  map[string][]string `json:"type_creators,omitempty"`
}

// schemaDelta is one section's added/removed members between two snapshots.
type schemaDelta struct {
	Section string   `json:"section"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

func newSchemaDriftCmd(flags *rootFlags) *cobra.Command {
	var deep bool
	var update bool
	var baselinePath string

	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Detect Zotero schema changes (new/removed item types and fields) vs a saved baseline",
		Long: `Capture a baseline fingerprint of the running Zotero's item-type and field
schema, then on later runs report what changed. Use this after upgrading Zotero
to see which item types, fields, or creator fields a new version added or removed
— the deltas the CLI may not yet model.

The first run captures the baseline. Re-run after an upgrade to see drift. Pass
--update to adopt the current live schema as the new baseline. The baseline is
stored at ~/.local/share/zotio/schema-baseline.json (override with
--baseline) and is shared across libraries because the schema is global to the
Zotero install.`,
		Example: `  # Capture a baseline on the current Zotero version
  zotio schema drift

  # After upgrading Zotero, see what changed
  zotio schema drift

  # Include per-item-type field/creator validity (many extra API calls)
  zotio schema drift --deep

  # Re-baseline to the current schema
  zotio schema drift --update`,
		// PATCH(glean mcp-surface-trim): mcp:hidden — schema drift is a
		// maintenance/ops task (baseline + compare after a Zotero upgrade).
		Annotations: map[string]string{"mcp:read-only": "true", "mcp:hidden": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Schema endpoints are global; newSchemaClient strips the library prefix.
			c, err := newSchemaClient(flags)
			if err != nil {
				return err
			}
			c.NoCache = true

			itemTypes, schemaVersion, err := probeSchemaVersion(c)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			path := baselinePath
			if path == "" {
				path = schemaBaselinePath()
			}
			base, ok, err := loadSchemaBaseline(path)
			if err != nil {
				return err
			}

			// First run: capture a full baseline.
			if !ok {
				live, err := completeSnapshot(c, itemTypes, schemaVersion, deep)
				if err != nil {
					return classifyAPIError(err, flags)
				}
				if err := saveSchemaBaseline(path, live); err != nil {
					return err
				}
				return renderSchemaDrift(cmd, flags, true, nil, path, live)
			}

			// Fast path: the Zotero-Schema-Version header covers the whole schema
			// (types, fields, per-type validity), so a matching version means no drift
			// at any depth — skip the remaining fetches. Only when both sides report a
			// version and we are not re-baselining.
			if !update && schemaVersion != "" && base.SchemaVersion == schemaVersion {
				base.SchemaVersion = schemaVersion
				return renderSchemaDrift(cmd, flags, false, nil, path, base)
			}

			live, err := completeSnapshot(c, itemTypes, schemaVersion, deep)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			deltas := diffSnapshots(base, live)
			if update {
				if err := saveSchemaBaseline(path, live); err != nil {
					return err
				}
			}
			return renderSchemaDrift(cmd, flags, false, deltas, path, live)
		},
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "Also diff per-item-type field and creator-type validity (many extra API calls)")
	cmd.Flags().BoolVar(&update, "update", false, "Adopt the current live schema as the new baseline after reporting")
	cmd.Flags().StringVar(&baselinePath, "baseline", "", "Baseline file path (default: ~/.local/share/zotio/schema-baseline.json)")
	return cmd
}

// probeSchemaVersion fetches the item-types list plus the Zotero-Schema-Version
// response header in a single request — enough to short-circuit a drift check when
// the schema version is unchanged. schemaVersion is empty when the header is absent.
func probeSchemaVersion(c *client.Client) (itemTypes []string, schemaVersion string, err error) {
	body, version, err := c.GetWithHeader("/itemTypes", nil, "Zotero-Schema-Version")
	if err != nil {
		return nil, "", err
	}
	itemTypes, err = decodeSchemaList(body, "/itemTypes", "itemType")
	return itemTypes, version, err
}

// completeSnapshot fills in the remaining schema lists (and, under deep, per-type
// fields and creator types) given the already-probed item types and schema version.
func completeSnapshot(c *client.Client, itemTypes []string, schemaVersion string, deep bool) (schemaSnapshot, error) {
	snap := schemaSnapshot{SchemaVersion: schemaVersion, ItemTypes: itemTypes}
	var err error
	if snap.ItemFields, err = fetchSchemaList(c, "/itemFields", nil, "field"); err != nil {
		return snap, err
	}
	if snap.CreatorFields, err = fetchSchemaList(c, "/creatorFields", nil, "field"); err != nil {
		return snap, err
	}
	if !deep {
		return snap, nil
	}
	snap.TypeFields = make(map[string][]string, len(itemTypes))
	snap.TypeCreators = make(map[string][]string, len(itemTypes))
	for _, it := range itemTypes {
		params := map[string]string{"itemType": it}
		tf, err := fetchSchemaList(c, "/itemTypeFields", params, "field")
		if err != nil {
			return snap, err
		}
		snap.TypeFields[it] = tf
		tc, err := fetchSchemaList(c, "/itemTypeCreatorTypes", params, "creatorType")
		if err != nil {
			return snap, err
		}
		snap.TypeCreators[it] = tc
	}
	return snap, nil
}

// fetchSchemaList GETs a Zotero schema endpoint returning an array of objects and
// extracts the string value of `key` from each, sorted and de-duplicated.
func fetchSchemaList(c *client.Client, path string, params map[string]string, key string) ([]string, error) {
	data, err := c.Get(path, params)
	if err != nil {
		return nil, err
	}
	return decodeSchemaList(data, path, key)
}

// decodeSchemaList extracts the sorted, de-duplicated string values of `key` from a
// Zotero schema array response. `path` is used only for error context.
func decodeSchemaList(data json.RawMessage, path, key string) ([]string, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parsing %s response: %w", path, err)
	}
	seen := make(map[string]struct{}, len(rows))
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		raw, present := row[key]
		if !present {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// diffSnapshots computes the ordered list of deltas from base to live. Per-type
// sections are emitted only for item types present in BOTH snapshots with deep
// data, so a wholly new/removed type shows up once (in item-types) rather than
// re-listing all its fields.
func diffSnapshots(base, live schemaSnapshot) []schemaDelta {
	var deltas []schemaDelta
	if d := diffStringSets("item-types", base.ItemTypes, live.ItemTypes); d != nil {
		deltas = append(deltas, *d)
	}
	if d := diffStringSets("item-fields", base.ItemFields, live.ItemFields); d != nil {
		deltas = append(deltas, *d)
	}
	if d := diffStringSets("creator-fields", base.CreatorFields, live.CreatorFields); d != nil {
		deltas = append(deltas, *d)
	}
	if base.TypeFields != nil && live.TypeFields != nil {
		for _, it := range sharedKeys(base.TypeFields, live.TypeFields) {
			if d := diffStringSets("type-fields:"+it, base.TypeFields[it], live.TypeFields[it]); d != nil {
				deltas = append(deltas, *d)
			}
		}
	}
	if base.TypeCreators != nil && live.TypeCreators != nil {
		for _, it := range sharedKeys(base.TypeCreators, live.TypeCreators) {
			if d := diffStringSets("type-creators:"+it, base.TypeCreators[it], live.TypeCreators[it]); d != nil {
				deltas = append(deltas, *d)
			}
		}
	}
	return deltas
}

// diffStringSets returns a delta of members added (in live, not base) and removed
// (in base, not live), or nil when the sets are identical.
func diffStringSets(section string, base, live []string) *schemaDelta {
	added := setDifference(live, base)
	removed := setDifference(base, live)
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	return &schemaDelta{Section: section, Added: added, Removed: removed}
}

// setDifference returns the sorted members of a that are not in b.
func setDifference(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, v := range b {
		inB[v] = struct{}{}
	}
	var out []string
	for _, v := range a {
		if _, ok := inB[v]; !ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// sharedKeys returns the sorted item types present in both maps.
func sharedKeys(a, b map[string][]string) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// schemaBaselinePath returns the default baseline location next to the local
// stores. It is intentionally not group-scoped: the Zotero schema is global to
// the install, identical across personal and group libraries.
func schemaBaselinePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "zotio", "schema-baseline.json")
}

// libraryPrefixRE matches the /users/<id> or /groups/<id> library segment of a
// Zotero API base URL.
var libraryPrefixRE = regexp.MustCompile(`/(users|groups)/[^/]+`)

// stripLibraryPrefix removes the library segment from a base URL so global
// schema endpoints resolve under /api directly (e.g.
// http://localhost:23119/api/users/0 -> http://localhost:23119/api).
func stripLibraryPrefix(baseURL string) string {
	return libraryPrefixRE.ReplaceAllString(baseURL, "")
}

// newSchemaClient builds a client whose base URL has the /users|groups/<id> library
// segment stripped, because Zotero's schema/type endpoints (itemTypes, itemFields,
// itemTypeFields, itemTypeCreatorTypes, creatorFields, items/new) are global, served
// under /api directly. The generated `schema *` commands 404 without this.
func newSchemaClient(flags *rootFlags) (*client.Client, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	c.BaseURL = stripLibraryPrefix(c.BaseURL)
	return c, nil
}

func loadSchemaBaseline(path string) (schemaSnapshot, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return schemaSnapshot{}, false, nil
	}
	if err != nil {
		return schemaSnapshot{}, false, fmt.Errorf("reading schema baseline: %w", err)
	}
	var snap schemaSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return schemaSnapshot{}, false, fmt.Errorf("parsing schema baseline %s: %w", path, err)
	}
	return snap, true, nil
}

func saveSchemaBaseline(path string, snap schemaSnapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating baseline directory: %w", err)
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	// #nosec G306 -- schema baselines are deterministic non-secret fixtures intended for review.
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing schema baseline: %w", err)
	}
	return nil
}

// renderSchemaDrift prints the result for both human and --json output. The
// delivered content is a pure function of the snapshots (no timestamps), so the
// same schema state yields the same output on repeated runs.
func renderSchemaDrift(cmd *cobra.Command, flags *rootFlags, captured bool, deltas []schemaDelta, path string, live schemaSnapshot) error {
	if flags.asJSON {
		return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"baseline_captured": captured,
			"drift":             len(deltas) > 0,
			"deltas":            deltas,
			"baseline_path":     path,
			"schema_version":    live.SchemaVersion,
			"item_types":        len(live.ItemTypes),
			"item_fields":       len(live.ItemFields),
			"creator_fields":    len(live.CreatorFields),
		}, flags)
	}

	w := cmd.OutOrStdout()
	if captured {
		fmt.Fprintf(w, "Schema baseline captured: %s\n", path)
		fmt.Fprintf(w, "  %d item types, %d item fields, %d creator fields", len(live.ItemTypes), len(live.ItemFields), len(live.CreatorFields))
		if live.SchemaVersion != "" {
			fmt.Fprintf(w, " (Zotero-Schema-Version %s)", live.SchemaVersion)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Re-run after upgrading Zotero to see what changed.")
		return nil
	}
	if len(deltas) == 0 {
		if live.SchemaVersion != "" {
			fmt.Fprintf(w, "No schema drift: Zotero-Schema-Version %s unchanged since baseline.\n", live.SchemaVersion)
		} else {
			fmt.Fprintln(w, "No schema drift: the live Zotero schema matches the saved baseline.")
		}
		return nil
	}
	fmt.Fprintf(w, "Schema drift detected in %d section(s):\n", len(deltas))
	for _, d := range deltas {
		for _, a := range d.Added {
			fmt.Fprintf(w, "  + %s: %s\n", d.Section, a)
		}
		for _, r := range d.Removed {
			fmt.Fprintf(w, "  - %s: %s\n", d.Section, r)
		}
	}
	return nil
}
