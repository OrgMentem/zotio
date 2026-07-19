// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// store-path seam (defaultDBPath), the `zotio demo` seeding lifecycle, the
// bundled fixture's research-integrity data, and the local-read hero commands
// (including reading-list --data-source local parity) reading the sandbox.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

// isolateDemoEnv gives the test an isolated HOME, pins ZOTIO_DEMO (so the demo
// command's own process-level os.Setenv("ZOTIO_DEMO","1") is reverted at
// cleanup and never poisons sibling tests' defaultDBPath), neutralizes the
// group scope, and points config + base URL at inert values. Returns the home.
func isolateDemoEnv(t *testing.T, demoVal string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTIO_DEMO", demoVal)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0") // unused in local mode
	t.Setenv("ZOTERO_QUEUE_TAG", "to-read")
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	return home
}

// runDemoCmd executes `zotio demo <args>` with JSON output and returns the
// decoded machine-readable report, failing the test on any command error.
func runDemoCmd(t *testing.T, args ...string) demoReport {
	t.Helper()
	cmd := newDemoCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("demo %v: %v; stderr=%s", args, err, errOut.String())
	}
	var report demoReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode demo report %q: %v", out.String(), err)
	}
	return report
}

// runReadCmd executes a read command with the given args and returns stdout,
// failing on any error. Not for commands that intentionally return a non-zero
// exit (e.g. a failed health gate) — those are run inline.
func runReadCmd(t *testing.T, cmd *cobra.Command, args []string) []byte {
	t.Helper()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s %v: %v; stderr=%s", cmd.Name(), args, err, errOut.String())
	}
	return out.Bytes()
}

// TestDefaultDBPathRoutesToDemoDBWhenActive defends the store-path safety seam:
// ZOTIO_DEMO in {1, true, ...} routes to demo.db regardless of the group scope,
// while {0, unset} preserves the exact real-store path (data.db, or the group
// suffix). A demo path must never be data.db and must never carry a group
// suffix; a real path must never be demo.db.
func TestDefaultDBPathRoutesToDemoDBWhenActive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedGroup := activeGroupID
	t.Cleanup(func() { activeGroupID = savedGroup })

	cases := []struct {
		name     string
		demo     string
		group    string
		wantDemo bool
	}{
		{"on_1", "1", "", true},
		{"on_1_group_ignored", "1", "999", true}, // group suffix must not interfere
		{"on_truthy", "true", "", true},          // any non-empty, non-"0" value is on
		{"off_zero", "0", "", false},
		{"off_zero_group", "0", "999", false},
		{"off_empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ZOTIO_DEMO", c.demo)
			activeGroupID = c.group
			got := defaultDBPath("zotio")
			base := filepath.Base(got)
			if c.wantDemo {
				if base != "demo.db" {
					t.Errorf("defaultDBPath = %q, want basename demo.db", got)
				}
				if strings.Contains(got, "data.db") || strings.Contains(got, "data-group") {
					t.Errorf("demo path %q leaked the real-store filename/group suffix", got)
				}
				return
			}
			wantFile := "data.db"
			if c.group != "" {
				wantFile = "data-group-" + c.group + ".db"
			}
			if base != wantFile {
				t.Errorf("defaultDBPath = %q, want basename %s", got, wantFile)
			}
			if strings.Contains(got, "demo.db") {
				t.Errorf("real-store path %q leaked demo.db", got)
			}
		})
	}
}

// TestDemoSeedCreatesSandboxWithFixtureCountsAndIsIdempotent covers the seeding
// lifecycle: an empty sandbox seeds to the fixture's row counts; a second run
// does not re-seed; --reset wipes and re-seeds; and seeding never creates the
// real data.db in the same HOME.
func TestDemoSeedCreatesSandboxWithFixtureCountsAndIsIdempotent(t *testing.T) {
	home := isolateDemoEnv(t, "0") // pin for restore; the command sets it to 1 during its run

	demoDB := filepath.Join(home, ".local", "share", "zotio", "demo.db")
	dataDB := filepath.Join(home, ".local", "share", "zotio", "data.db")

	// First run seeds an empty sandbox.
	r1 := runDemoCmd(t)
	if !r1.Seeded {
		t.Errorf("first run Seeded = false, want true (empty sandbox must seed)")
	}
	if r1.Items != 34 {
		t.Errorf("first run Items = %d, want 34 top-level papers", r1.Items)
	}
	if filepath.Base(r1.DBPath) != "demo.db" {
		t.Errorf("report DBPath = %q, want basename demo.db", r1.DBPath)
	}
	if _, err := os.Stat(demoDB); err != nil {
		t.Fatalf("sandbox demo.db was not created: %v", err)
	}

	// The fixture's rows all land: every item (incl. attachments/annotations)
	// and every collection.
	db, err := store.OpenWithContext(context.Background(), demoDB)
	if err != nil {
		t.Fatalf("open seeded sandbox: %v", err)
	}
	qs := localQueryStore{db}
	if got := countResources(t, qs, "items"); got != 52 {
		t.Errorf("seeded item rows = %d, want 52 (per fixture)", got)
	}
	if got := countResources(t, qs, "collections"); got != 3 {
		t.Errorf("seeded collection rows = %d, want 3 (per fixture)", got)
	}
	_ = db.Close()

	// Second run is idempotent: it must NOT re-seed.
	r2 := runDemoCmd(t)
	if r2.Seeded {
		t.Errorf("second run Seeded = true, want false (already-seeded sandbox must not re-seed)")
	}
	if r2.Items != 34 {
		t.Errorf("second run Items = %d, want 34", r2.Items)
	}

	// --reset wipes and re-seeds from scratch.
	r3 := runDemoCmd(t, "--reset")
	if !r3.Seeded {
		t.Errorf("--reset run Seeded = false, want true (reset must re-seed)")
	}
	if r3.Items != 34 {
		t.Errorf("--reset run Items = %d, want 34", r3.Items)
	}

	// Seeding the sandbox must never create/touch the real store.
	if _, err := os.Stat(dataDB); !os.IsNotExist(err) {
		t.Errorf("seeding the sandbox created or touched the real store %q (stat err = %v)", dataDB, err)
	}
}

// TestDemoFixtureContainsRequiredResearchIntegrityData pins the bundled
// fixture's load-bearing data — the specific DOIs, citekeys, and tag variants
// the hero commands rely on — through the very loaders/queries those commands
// use, over a freshly seeded store.
func TestDemoFixtureContainsRequiredResearchIntegrityData(t *testing.T) {
	qs := seedFixtureStore(t)

	t.Run("wakefield_retraction_doi_present", func(t *testing.T) {
		const doi = "10.1016/S0140-6736(97)11096-0"
		rows, err := qs.QueryRaw(
			`SELECT id AS key FROM resources WHERE resource_type='items' AND json_extract(data,'$.data.DOI') = ?`, doi)
		if err != nil {
			t.Fatalf("DOI query: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("items carrying the Wakefield DOI %s = %d, want exactly 1", doi, len(rows))
		}
	})

	t.Run("smith2020data_conflict_pair", func(t *testing.T) {
		items, err := loadCitekeyItems(qs)
		if err != nil {
			t.Fatalf("loadCitekeyItems: %v", err)
		}
		var keys []string
		for _, it := range items {
			if it.CiteKey == "smith2020data" {
				keys = append(keys, it.Key)
			}
		}
		sort.Strings(keys)
		want := []string{"FAIR002016", "TIDYDATA14"}
		if !reflect.DeepEqual(keys, want) {
			t.Errorf("items sharing citekey smith2020data = %v, want %v (a genuine conflict pair)", keys, want)
		}
	})

	t.Run("pinned_citekeys_resolve", func(t *testing.T) {
		items, err := loadCitekeyItems(qs)
		if err != nil {
			t.Fatalf("loadCitekeyItems: %v", err)
		}
		byCiteKey := map[string]string{}
		for _, it := range items {
			if it.CiteKey != "" {
				byCiteKey[it.CiteKey] = it.Key
			}
		}
		for citekey, wantKey := range map[string]string{
			"vaswani2017attention": "ATTENTION1",
			"devlin2019bert":       "BERT000019",
			"he2016resnet":         "RESNET0016",
		} {
			if got := byCiteKey[citekey]; got != wantKey {
				t.Errorf("citekey %q resolved to item %q, want %q", citekey, got, wantKey)
			}
		}
	})

	t.Run("duplicate_doi_pair", func(t *testing.T) {
		rows, err := queryDuplicateDOIs(qs)
		if err != nil {
			t.Fatalf("queryDuplicateDOIs: %v", err)
		}
		// queryDuplicateDOIs lower-cases the DOI for grouping.
		got := doiDupKeys(t, rows, "10.48550/arxiv.1406.2661")
		sort.Strings(got)
		want := []string{"GANDUP0014", "GANORIG014"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("duplicate-DOI group keys = %v, want %v", got, want)
		}
	})

	t.Run("tag_drift_variants_grouped", func(t *testing.T) {
		tagRows, err := qs.QueryRaw(tagAuditDistinctQuery)
		if err != nil {
			t.Fatalf("tag distinct query: %v", err)
		}
		countRows, err := qs.QueryRaw(tagAuditCountQuery)
		if err != nil {
			t.Fatalf("tag count query: %v", err)
		}
		variants := map[string]map[string]bool{} // normalized -> set of raw variants
		for _, p := range buildTagAuditPlans(tagRows, countRows) {
			set := map[string]bool{p.Canonical: true}
			for _, a := range p.Aliases {
				set[a] = true
			}
			variants[normalizeTagAuditName(p.Canonical)] = set
		}
		for norm, want := range map[string][]string{
			"deep learning":    {"Deep Learning", "deep learning"},
			"machine learning": {"Machine Learning", "machine learning"},
			"computer vision":  {"Computer Vision", "computer vision"},
		} {
			set := variants[norm]
			if set == nil {
				t.Errorf("no tag-drift group for normalized %q", norm)
				continue
			}
			for _, v := range want {
				if !set[v] {
					t.Errorf("tag-drift group %q missing variant %q (got %v)", norm, v, set)
				}
			}
		}
		// Boundary: "machine-learning" normalizes to its own key (a hyphen is
		// not whitespace), so it must NOT fold into the "machine learning"
		// drift group.
		if variants["machine learning"]["machine-learning"] {
			t.Error(`hyphenated "machine-learning" was wrongly folded into the "machine learning" drift group`)
		}
	})
}

// TestDemoCommandsReadSeededSandboxEndToEnd proves the hero read commands,
// under ZOTIO_DEMO=1, route to and surface findings from the seeded sandbox.
func TestDemoCommandsReadSeededSandboxEndToEnd(t *testing.T) {
	isolateDemoEnv(t, "1")
	if r := runDemoCmd(t); !r.Seeded || r.Items != 34 {
		t.Fatalf("seed precondition failed: %+v", r)
	}

	t.Run("items_citekey_conflicts", func(t *testing.T) {
		out := runReadCmd(t, newItemsCitekeyConflictsCmd(&rootFlags{asJSON: true}), []string{"--conflicts"})
		var report FindingsReport
		if err := json.Unmarshal(out, &report); err != nil {
			t.Fatalf("decode conflicts %q: %v", out, err)
		}
		n := 0
		for _, f := range report.Findings {
			if f.Kind == "citekey_conflict" && f.Evidence["cite_key"] == "smith2020data" {
				n++
			}
		}
		if n != 2 {
			t.Errorf("citekey-conflicts --conflicts reported smith2020data on %d items, want 2; findings=%v", n, report.Findings)
		}
	})

	t.Run("library_health_for_citation", func(t *testing.T) {
		cmd := newLibraryHealthCmd(&rootFlags{asJSON: true})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs([]string{"--for", "citation"})
		// The citation preset fails-on "high" and the sandbox holds a critical
		// citekey conflict, so the gate exits 11 AFTER printing the report. Any
		// other error is a real failure.
		if err := cmd.Execute(); err != nil {
			var cerr *cliError
			if !errors.As(err, &cerr) || cerr.code != 11 {
				t.Fatalf("library health --for citation errored non-gate: %v; stderr=%s", err, errOut.String())
			}
		}
		var report healthReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("decode health report %q: %v", out.String(), err)
		}
		if !hasFindingKind(report, "citekey_conflict") {
			t.Errorf("library health --for citation has no citekey_conflict finding; findings=%+v", report.Findings)
		}
	})

	t.Run("search_attention_local", func(t *testing.T) {
		out := runReadCmd(t, newSearchCmd(&rootFlags{asJSON: true, dataSource: "local"}), []string{"attention"})
		var env struct {
			Results []json.RawMessage `json:"results"`
			Meta    map[string]any    `json:"meta"`
		}
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("decode search %q: %v", out, err)
		}
		if src, _ := env.Meta["source"].(string); src != "local" {
			t.Errorf("search provenance source = %q, want local", src)
		}
		if len(env.Results) == 0 {
			t.Fatalf("search 'attention' --data-source local returned no results")
		}
		if !anyResultContains(env.Results, "ATTENTION1") {
			t.Errorf("search 'attention' did not surface the ATTENTION1 paper; %d results", len(env.Results))
		}
	})

	t.Run("reading_list_local_demo_mode", func(t *testing.T) {
		res := runReadingListLocal(t)
		assertToReadQueue(t, res)
	})
}

// TestReadingListLocalParityAgainstNormalSeededStore proves the reading-list
// --data-source local path is not demo-specific: it works over a normal synced
// data.db too, filtering by the queue tag and returning the oldest-first queue.
func TestReadingListLocalParityAgainstNormalSeededStore(t *testing.T) {
	isolateDemoEnv(t, "0") // normal store, not the sandbox

	dataDB := defaultDBPath("zotio") // demo off + no group => data.db
	if filepath.Base(dataDB) != "data.db" {
		t.Fatalf("precondition: defaultDBPath = %q, want data.db", dataDB)
	}
	if err := os.MkdirAll(filepath.Dir(dataDB), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := seedDemoStore(context.Background(), dataDB); err != nil {
		t.Fatalf("seed data.db: %v", err)
	}

	assertToReadQueue(t, runReadingListLocal(t))
}

func TestSeedDemoStoreReturnsSyncStatePersistenceError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "demo.db")
	if _, err := seedDemoStore(context.Background(), dbPath); err != nil {
		t.Fatalf("initial seedDemoStore: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open seeded store: %v", err)
	}
	if _, err := db.DB().Exec(`
CREATE TRIGGER reject_sync_state
BEFORE INSERT ON sync_state
BEGIN
	SELECT RAISE(ABORT, 'sync state unavailable');
END`); err != nil {
		_ = db.Close()
		t.Fatalf("create sync state failure trigger: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	_, err = seedDemoStore(context.Background(), dbPath)
	if err == nil {
		t.Fatal("seedDemoStore succeeded despite sync-state persistence failure")
	}
	if !strings.Contains(err.Error(), "recording collections sync state") || !strings.Contains(err.Error(), "sync state unavailable") {
		t.Errorf("seedDemoStore error = %v, want wrapped sync-state persistence failure", err)
	}
}

// --- shared helpers ---

func seedFixtureStore(t *testing.T) localQueryStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "demo.db")
	if _, err := seedDemoStore(context.Background(), dbPath); err != nil {
		t.Fatalf("seedDemoStore: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open seeded store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return localQueryStore{db}
}

func countResources(t *testing.T, qs localQueryStore, resourceType string) int {
	t.Helper()
	rows, err := qs.QueryRaw(`SELECT COUNT(*) AS n FROM resources WHERE resource_type = ?`, resourceType)
	if err != nil {
		t.Fatalf("count %s: %v", resourceType, err)
	}
	if len(rows) == 0 {
		return 0
	}
	return sqlIntValue(rows[0]["n"])
}

func doiDupKeys(t *testing.T, rows []map[string]any, wantValue string) []string {
	t.Helper()
	for _, row := range rows {
		if sqlStringValue(row["value"]) != wantValue {
			continue
		}
		var keys []string
		if err := json.Unmarshal([]byte(sqlStringValue(row["keys"])), &keys); err != nil {
			t.Fatalf("parse dup keys %v: %v", row["keys"], err)
		}
		return keys
	}
	t.Fatalf("no duplicate-DOI group with value %q in %v", wantValue, rows)
	return nil
}

func hasFindingKind(report healthReport, kind string) bool {
	for _, f := range report.Findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

func anyResultContains(results []json.RawMessage, needle string) bool {
	for _, r := range results {
		if strings.Contains(string(r), needle) {
			return true
		}
	}
	return false
}

// runReadingListLocal runs `zotio reading-list --data-source local` (JSON) and
// decodes the queue result.
func runReadingListLocal(t *testing.T) readingListResult {
	t.Helper()
	out := runReadCmd(t, newReadingListCmd(&rootFlags{asJSON: true, dataSource: "local"}), nil)
	var res readingListResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode reading-list %q: %v", out, err)
	}
	return res
}

// assertToReadQueue pins the fixture's to-read queue: five papers, tagged
// "to-read", oldest-first (ATTENTION1, added 2024-01-12).
func assertToReadQueue(t *testing.T, res readingListResult) {
	t.Helper()
	if res.QueueTag != "to-read" {
		t.Errorf("QueueTag = %q, want to-read", res.QueueTag)
	}
	if res.Count != 5 {
		t.Errorf("reading-list --data-source local Count = %d, want 5 to-read papers", res.Count)
	}
	if res.Oldest != "2024-01-12T13:30:00Z" {
		t.Errorf("Oldest = %q, want the earliest to-read dateAdded 2024-01-12T13:30:00Z", res.Oldest)
	}
}
