// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/store"
)

func TestCreatorsAuditVariantTierClassification(t *testing.T) {
	t.Run("exact normalization keeps fuller canonical", func(t *testing.T) {
		groups := buildCreatorVariantGroups([]*creatorOccurrence{
			creatorAuditTestOccurrence("John Smith", "J1"),
			creatorAuditTestOccurrence("John Smith", "J2"),
			creatorAuditTestOccurrence("john  smith", "J3"),
		})

		group := creatorAuditRequireOnlyGroup(t, groups, creatorVariantTierExact)
		if group.Canonical != "John Smith" {
			t.Fatalf("canonical = %q, want John Smith", group.Canonical)
		}
		creatorAuditRequireAlias(t, group, "john smith", 1)
	})

	t.Run("single initial with full name is initials compatible", func(t *testing.T) {
		groups := buildCreatorVariantGroups([]*creatorOccurrence{
			creatorAuditTestOccurrence("J. Smith", "I1"),
			creatorAuditTestOccurrence("John Smith", "I2"),
		})

		group := creatorAuditRequireOnlyGroup(t, groups, creatorVariantTierInitials)
		if group.Canonical != "John Smith" {
			t.Fatalf("canonical = %q, want fuller John Smith on count tie", group.Canonical)
		}
		creatorAuditRequireAlias(t, group, "J. Smith", 1)
	})

	t.Run("different initials for uncommon surname do not group", func(t *testing.T) {
		groups := buildCreatorVariantGroups([]*creatorOccurrence{
			creatorAuditTestOccurrence("A. Smith", "A1"),
			creatorAuditTestOccurrence("B. Smith", "B1"),
		})
		if len(groups) != 0 {
			t.Fatalf("groups = %+v, want no group for A. Smith vs B. Smith", groups)
		}
	})

	t.Run("middle initials remain compatible", func(t *testing.T) {
		groups := buildCreatorVariantGroups([]*creatorOccurrence{
			creatorAuditTestOccurrence("J. R. Smith", "M1"),
			creatorAuditTestOccurrence("John R. Smith", "M2"),
		})

		group := creatorAuditRequireOnlyGroup(t, groups, creatorVariantTierInitials)
		if group.Canonical != "John R. Smith" {
			t.Fatalf("canonical = %q, want fuller John R. Smith on count tie", group.Canonical)
		}
		creatorAuditRequireAlias(t, group, "J. R. Smith", 1)
	})
}

func TestCreatorsAuditCanonicalSelection(t *testing.T) {
	t.Run("highest item count wins before fullness", func(t *testing.T) {
		groups := buildCreatorVariantGroups([]*creatorOccurrence{
			creatorAuditTestOccurrence("J. Count", "C1"),
			creatorAuditTestOccurrence("J. Count", "C2"),
			creatorAuditTestOccurrence("John Count", "C3"),
		})

		group := creatorAuditRequireOnlyGroup(t, groups, creatorVariantTierInitials)
		if group.Canonical != "J. Count" || group.CanonicalItemCount != 2 {
			t.Fatalf("canonical = %q/%d, want J. Count/2", group.Canonical, group.CanonicalItemCount)
		}
		creatorAuditRequireAlias(t, group, "John Count", 1)
	})

	t.Run("count tie chooses fuller longer form", func(t *testing.T) {
		groups := buildCreatorVariantGroups([]*creatorOccurrence{
			creatorAuditTestOccurrence("J. Fuller", "F1"),
			creatorAuditTestOccurrence("John Fuller", "F2"),
		})

		group := creatorAuditRequireOnlyGroup(t, groups, creatorVariantTierInitials)
		if group.Canonical != "John Fuller" || group.CanonicalItemCount != 1 {
			t.Fatalf("canonical = %q/%d, want John Fuller/1", group.Canonical, group.CanonicalItemCount)
		}
		creatorAuditRequireAlias(t, group, "J. Fuller", 1)
	})
}

func TestCreatorsAuditJSONContractIncludesGroupsAndFindingsEnvelope(t *testing.T) {
	creatorAuditSeedCommandStore(t,
		creatorAuditTestItem("JC1", "Contract One", "", nil, creatorAuditNameCreator("John Contract")),
		creatorAuditTestItem("JC2", "Contract Two", "", nil, creatorAuditNameCreator("john  contract")),
	)

	report, raw := creatorAuditRunJSONCommand(t)
	if !bytes.Contains(raw, []byte(`"groups"`)) {
		t.Fatalf("JSON output %s missing groups array", string(raw))
	}
	if len(report.Groups) != 1 {
		t.Fatalf("groups = %+v, want one exact group", report.Groups)
	}
	group := report.Groups[0]
	if group.Tier != creatorVariantTierExact || group.Canonical != "John Contract" {
		t.Fatalf("group = %+v, want exact canonical John Contract", group)
	}

	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want one finding envelope entry", report.Findings)
	}
	finding := report.Findings[0]
	if finding.Kind != string(creatorVariantTierExact) {
		t.Fatalf("finding kind = %q, want %q", finding.Kind, creatorVariantTierExact)
	}
	if finding.Evidence["canonical"] != "John Contract" {
		t.Fatalf("finding evidence canonical = %#v, want John Contract", finding.Evidence["canonical"])
	}
	aliases, ok := finding.Evidence["aliases"].([]any)
	if !ok || len(aliases) != 1 {
		t.Fatalf("finding aliases = %#v, want one alias", finding.Evidence["aliases"])
	}
	alias, ok := aliases[0].(map[string]any)
	if !ok || alias["name"] != "john contract" {
		t.Fatalf("finding alias = %#v, want name john contract", aliases[0])
	}
}

func TestCreatorsAuditORCIDSidecarAndEvidence(t *testing.T) {
	dbPath := creatorAuditSeedCommandStore(t,
		creatorAuditTestItem("L1", "Lee Initial", "10.555/lee-initial", nil, creatorAuditNameCreator("A. Lee")),
		creatorAuditTestItem("L2", "Lee Full", "10.555/lee-full", nil, creatorAuditNameCreator("Alice Lee")),
		creatorAuditTestItem("S1", "Smith Initial", "10.555/smith-initial", nil, creatorAuditNameCreator("J. Smith")),
		creatorAuditTestItem("S2", "Smith Full", "10.555/smith-full", nil, creatorAuditNameCreator("John Smith")),
	)

	var hits atomic.Int32
	crossref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		doi, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/works/"))
		if err != nil {
			t.Errorf("unescape CrossRef path %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad DOI", http.StatusBadRequest)
			return
		}
		author, ok := map[string][3]string{
			"10.555/lee-initial":   {"A.", "Lee", "https://orcid.org/0000-0001-0002-0003"},
			"10.555/lee-full":      {"Alice", "Lee", "0000-0001-0002-0003"},
			"10.555/smith-initial": {"J.", "Smith", "0000-0002-0000-0000"},
			"10.555/smith-full":    {"John", "Smith", "0000-0003-0000-0000"},
		}[doi]
		if !ok {
			t.Errorf("unexpected CrossRef DOI lookup %q", doi)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		given, family, orcid := author[0], author[1], author[2]
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"message":{"author":[{"given":%q,"family":%q,"ORCID":%q}]}}`, given, family, orcid)
	}))
	t.Cleanup(crossref.Close)
	withBase(t, &enrichCrossRefBase, crossref.URL)

	reportWithoutORCID, _ := creatorAuditRunJSONCommand(t)
	if reportWithoutORCID.Summary.ORCID != nil {
		t.Fatalf("summary ORCID = %+v, want nil without --orcid", reportWithoutORCID.Summary.ORCID)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("CrossRef hits without --orcid = %d, want 0", got)
	}

	report, _ := creatorAuditRunJSONCommand(t, "--orcid")
	if got := hits.Load(); got != 4 {
		t.Fatalf("CrossRef hits with --orcid = %d, want one lookup per DOI-bearing candidate item", got)
	}
	if report.Summary.ORCID == nil || !report.Summary.ORCID.Enabled || report.Summary.ORCID.Lookups != 4 || report.Summary.ORCID.Captured != 4 || report.Summary.ORCID.Failed != 0 {
		t.Fatalf("ORCID summary = %+v, want enabled/lookups=4/captured=4/failed=0", report.Summary.ORCID)
	}

	rows := creatorAuditReadORCIDRows(t, dbPath)
	wantRows := map[string]string{
		"L1": "https://orcid.org/0000-0001-0002-0003",
		"L2": "https://orcid.org/0000-0001-0002-0003",
		"S1": "https://orcid.org/0000-0002-0000-0000",
		"S2": "https://orcid.org/0000-0003-0000-0000",
	}
	if len(rows) != len(wantRows) {
		t.Fatalf("creator_orcids rows = %+v, want %d rows", rows, len(wantRows))
	}
	for key, want := range wantRows {
		if got := rows[key]; got != want {
			t.Fatalf("creator_orcids[%s] = %q, want %q; rows=%+v", key, got, want, rows)
		}
	}

	lee := creatorAuditRequireGroupContaining(t, report.Groups, "Alice Lee", "A. Lee")
	if lee.Tier != creatorVariantTierInitials {
		t.Fatalf("Lee tier = %q, want initials after same-ORCID corroboration", lee.Tier)
	}
	if matches, ok := lee.Evidence["orcid_matches"].([]any); !ok || len(matches) == 0 {
		t.Fatalf("Lee evidence = %+v, want non-empty orcid_matches", lee.Evidence)
	}

	smith := creatorAuditRequireGroupContaining(t, report.Groups, "John Smith", "J. Smith")
	if smith.Tier != creatorVariantTierAmbiguous {
		t.Fatalf("Smith tier = %q, want ambiguous after conflicting ORCIDs", smith.Tier)
	}
	if conflicts, ok := smith.Evidence["orcid_conflicts"].([]any); !ok || len(conflicts) == 0 {
		t.Fatalf("Smith evidence = %+v, want non-empty orcid_conflicts", smith.Evidence)
	}
}

func TestCreatorsAuditScopeLimitsInventory(t *testing.T) {
	creatorAuditSeedCommandStore(t,
		creatorAuditTestItem("SC1", "Scoped Initial", "", []string{"COLX"}, creatorAuditNameCreator("J. Scope")),
		creatorAuditTestItem("SC2", "Scoped Full", "", []string{"COLX"}, creatorAuditNameCreator("John Scope")),
		creatorAuditTestItem("OUT1", "Outside Initial", "", nil, creatorAuditNameCreator("J. Outside")),
		creatorAuditTestItem("OUT2", "Outside Full", "", nil, creatorAuditNameCreator("John Outside")),
	)

	report, _ := creatorAuditRunJSONCommand(t, "--scope", "collection:COLX")
	if report.Summary.Scope != "collection:COLX" || report.Summary.ItemCount != 2 {
		t.Fatalf("summary = %+v, want scope collection:COLX and two in-scope items", report.Summary)
	}
	if len(report.Groups) != 1 {
		t.Fatalf("groups = %+v, want only the scoped Scope group", report.Groups)
	}
	group := creatorAuditRequireGroupContaining(t, report.Groups, "John Scope", "J. Scope")
	if group.Tier != creatorVariantTierInitials {
		t.Fatalf("scoped group tier = %q, want initials", group.Tier)
	}
	for _, group := range report.Groups {
		if strings.Contains(group.Canonical, "Outside") {
			t.Fatalf("out-of-scope group leaked into report: %+v", group)
		}
		for _, alias := range group.Aliases {
			if strings.Contains(alias.Name, "Outside") {
				t.Fatalf("out-of-scope alias leaked into report: %+v", group)
			}
		}
	}
}

func TestCreatorsAuditSurfacesSyncStateReadFailure(t *testing.T) {
	dbPath := creatorAuditSeedCommandStore(t,
		creatorAuditTestItem("SS1", "Sync State", "", nil, creatorAuditNameCreator("Ada Lovelace")),
	)

	// Poison sync-state metadata so GetSyncState("items") returns a genuine
	// read error (an unparseable last_synced_at fails the scan into
	// time.Time) rather than the ErrNoRows "never synced" case. Migrations
	// recreate the table/columns on open, but leave row data untouched.
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if _, err := db.DB().ExecContext(context.Background(),
		`INSERT INTO sync_state (resource_type, last_synced_at) VALUES ('items', 'not-a-timestamp')
		 ON CONFLICT(resource_type) DO UPDATE SET last_synced_at = excluded.last_synced_at`,
	); err != nil {
		_ = db.Close()
		t.Fatalf("poison sync_state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	flags := &rootFlags{asJSON: true, timeout: time.Second}
	cmd := newCreatorsAuditCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(nil)

	err = cmd.Execute()
	if err == nil {
		t.Fatalf("expected sync-state read failure to surface, got success: stdout=%s", out.String())
	}
	if !strings.Contains(err.Error(), "sync state") {
		t.Fatalf("error = %v, want it to reference the sync state failure", err)
	}
}

func creatorAuditTestOccurrence(name, itemKey string) *creatorOccurrence {
	display, first, last := creatorDisplayParts("", "", name)
	normFirst := normalizeCreatorAuditText(first)
	normFull := normalizeCreatorAuditText(display)
	return &creatorOccurrence{
		ItemKey:     itemKey,
		Name:        collapseCreatorWhitespace(name),
		FirstName:   first,
		LastName:    last,
		DisplayName: display,
		NormFull:    normFull,
		NormFirst:   normFirst,
		NormLast:    normalizeCreatorAuditText(last),
		FirstTokens: creatorAuditTokens(normFirst),
		NameHash:    creatorAuditNameHash(normFull),
	}
}

func creatorAuditRequireOnlyGroup(t *testing.T, groups []creatorVariantGroup, tier creatorVariantTier) creatorVariantGroup {
	t.Helper()
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want exactly one group", groups)
	}
	if groups[0].Tier != tier {
		t.Fatalf("tier = %q, want %q; group=%+v", groups[0].Tier, tier, groups[0])
	}
	return groups[0]
}

func creatorAuditRequireAlias(t *testing.T, group creatorVariantGroup, name string, itemCount int) {
	t.Helper()
	for _, alias := range group.Aliases {
		if alias.Name == name {
			if alias.ItemCount != itemCount {
				t.Fatalf("alias %q count = %d, want %d", name, alias.ItemCount, itemCount)
			}
			return
		}
	}
	t.Fatalf("alias %q not found in %+v", name, group.Aliases)
}

func creatorAuditSeedCommandStore(t *testing.T, items ...json.RawMessage) string {
	t.Helper()
	creatorAuditIsolateEnv(t)
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return dbPath
}

func creatorAuditIsolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
}

func creatorAuditRunJSONCommand(t *testing.T, args ...string) (creatorsAuditReport, []byte) {
	t.Helper()
	flags := &rootFlags{asJSON: true, timeout: time.Second}
	cmd := newCreatorsAuditCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creators audit %v: %v; stderr=%s", args, err, errOut.String())
	}
	var report creatorsAuditReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode creators audit JSON %q: %v", out.String(), err)
	}
	return report, out.Bytes()
}

func creatorAuditTestItem(key, title, doi string, collections []string, creators ...map[string]string) json.RawMessage {
	data := map[string]any{
		"key":       key,
		"itemType":  "journalArticle",
		"title":     title,
		"creators":  creators,
		"dateAdded": "2026-01-01T00:00:00Z",
	}
	if doi != "" {
		data["DOI"] = doi
	}
	if len(collections) > 0 {
		data["collections"] = collections
	}
	item := map[string]any{"key": key, "version": 1, "data": data}
	raw, err := json.Marshal(item)
	if err != nil {
		panic(err)
	}
	return raw
}

func creatorAuditNameCreator(name string) map[string]string {
	return map[string]string{"creatorType": "author", "name": name}
}

func creatorAuditReadORCIDRows(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	rows, err := db.DB().QueryContext(context.Background(), `SELECT item_key, orcid FROM creator_orcids ORDER BY item_key`)
	if err != nil {
		t.Fatalf("query creator_orcids: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var key, orcid string
		if err := rows.Scan(&key, &orcid); err != nil {
			t.Fatalf("scan creator_orcids: %v", err)
		}
		out[key] = orcid
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("creator_orcids rows: %v", err)
	}
	return out
}

func creatorAuditRequireGroupContaining(t *testing.T, groups []creatorVariantGroup, canonical string, alias string) creatorVariantGroup {
	t.Helper()
	for _, group := range groups {
		if group.Canonical != canonical {
			continue
		}
		for _, gotAlias := range group.Aliases {
			if gotAlias.Name == alias {
				return group
			}
		}
	}
	t.Fatalf("group canonical %q alias %q not found in %+v", canonical, alias, groups)
	return creatorVariantGroup{}
}
