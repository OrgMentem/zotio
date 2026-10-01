// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
)

// ahParent is a top-level item complete for every metadata check in `all`, so
// a fixture's findings come from attachment storage alone.
func ahParent(key, collection string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"key":%[1]q,"version":1,"data":{"key":%[1]q,"itemType":"journalArticle","title":"AH paper %[1]s","creators":[{"lastName":"Doe%[1]s"}],"date":"2020","publicationTitle":"Journal","DOI":"10/ah-%[1]s","abstractNote":"abs","tags":[{"tag":"ahtag"}],"extra":"Citation Key: ah%[1]s","collections":[%[2]q]}}`,
		key, collection))
}

// ahAttachment builds an attachment child. size < 0 omits links.enclosure.
func ahAttachment(key, parent, linkMode, contentType, md5 string, size int) json.RawMessage {
	links := ""
	if size >= 0 {
		links = fmt.Sprintf(`"links":{"enclosure":{"length":%d}},`, size)
	}
	path := ""
	if linkMode == "linked_file" {
		path = fmt.Sprintf(`,"path":"/Users/me/papers/%s.pdf"`, key)
	}
	return json.RawMessage(fmt.Sprintf(
		`{"key":%[1]q,"version":1,%[2]s"data":{"key":%[1]q,"itemType":"attachment","parentItem":%[3]q,"linkMode":%[4]q,"contentType":%[5]q,"filename":"%[1]s.pdf","md5":%[6]q%[7]s}}`,
		key, links, parent, linkMode, contentType, md5, path))
}

// ahPortabilityItems: PI owns a stored PDF, PL a linked_file PDF, PU only a
// linked_url web link.
func ahPortabilityItems(plLinkMode string) []json.RawMessage {
	return []json.RawMessage{
		ahParent("PI", "COLN"),
		ahAttachment("PIA", "PI", "imported_file", "application/pdf", "", -1),
		ahParent("PL", "COLN"),
		ahAttachment("PLA", "PL", plLinkMode, "application/pdf", "", -1),
		ahParent("PU", "COLN"),
		ahAttachment("PUA", "PU", "linked_url", "text/html", "", -1),
	}
}

func ahStore(t *testing.T, items []json.RawMessage) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), t.TempDir()+"/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return localQueryStore{db}
}

func ahFindings(report healthReport, kind string) []Finding {
	out := []Finding{}
	for _, f := range report.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func TestAHNonportableAttachmentOnlyLinkedFilePDF(t *testing.T) {
	db := ahStore(t, ahPortabilityItems("linked_file"))
	report, err := assembleHealthReport(db, newHealthCtx("all", false), "all", healthPresets["all"], "", scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	got := ahFindings(report, "nonportable_attachment")
	if len(got) != 1 || got[0].ItemKey != "PL" {
		t.Fatalf("nonportable_attachment = %+v, want exactly parent PL", got)
	}
	ev := got[0].Evidence
	if ev["attachment"] != "PLA" || ev["path"] != "/Users/me/papers/PLA.pdf" {
		t.Errorf("evidence = %v, want attachment PLA and its linked path", ev)
	}
	if got[0].Severity == sevHigh || got[0].Severity == sevCritical {
		t.Errorf("severity = %q, must stay below high", got[0].Severity)
	}
	if got[0].RecommendedAction == nil || got[0].RecommendedAction.Command != "" {
		t.Errorf("action = %+v, want review text and no runnable upload command", got[0].RecommendedAction)
	}
	if _, ok := ev["library"]; ok {
		t.Errorf("personal-library finding carries library evidence %v", ev["library"])
	}
	// missing_pdf is unchanged: any PDF child, linked or stored, satisfies it;
	// the linked_url web link does not.
	if keys := findingItemKeys(report, "missing_pdf"); !slices.Equal(keys, []string{"PU"}) {
		t.Errorf("missing_pdf keys = %v, want [PU]", keys)
	}
}

func TestAHNonportableAttachmentScopeAndGroupLibrary(t *testing.T) {
	db := ahStore(t, ahPortabilityItems("linked_file"))

	scoped, err := assembleHealthReport(db, newHealthCtx("all", false), "all", healthPresets["all"], "", scopeResult{Keys: []string{"PI", "PU"}, Expr: "item:PI,PU"})
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if got := ahFindings(scoped, "nonportable_attachment"); len(got) != 0 {
		t.Errorf("scope without PL reported %+v", got)
	}

	ctx := newHealthCtx("all", false)
	ctx.flags = &rootFlags{group: "4242"}
	grouped, err := assembleHealthReport(db, ctx, "all", healthPresets["all"], "", scopeResult{Keys: []string{"PL"}, Expr: "item:PL"})
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	got := ahFindings(grouped, "nonportable_attachment")
	if len(got) != 1 || got[0].Evidence["library"] != "group:4242" {
		t.Fatalf("group finding = %+v, want library group:4242", got)
	}
	if !strings.Contains(got[0].RecommendedAction.Text, "group") {
		t.Errorf("group remediation %q does not address group members", got[0].RecommendedAction.Text)
	}
}

// The new checks sit below high: a high gate ignores them, an info gate does not.
func TestAHNewAttachmentChecksStayBelowHighGate(t *testing.T) {
	items := append(ahPortabilityItems("linked_file"),
		ahAttachment("DUP1", "PI", "imported_file", "application/pdf", "abc", 10),
		ahAttachment("DUP2", "PI", "imported_file", "application/pdf", "abc", 10),
	)
	db := ahStore(t, items)
	kinds := []string{"nonportable_attachment", duplicateAttachmentBytesKind}

	high, err := assembleHealthReport(db, newHealthCtx("all", false), "all", kinds, sevHigh, scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("high: %v", err)
	}
	if high.Summary.Total != 2 || high.Gate.Status != "passed" || healthGateExitError(high) != nil {
		t.Fatalf("high gate = %+v summary %+v, want passed with 2 findings", high.Gate, high.Summary)
	}
	info, err := assembleHealthReport(db, newHealthCtx("all", false), "all", kinds, sevInfo, scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if code := ExitCode(healthGateExitError(info)); code != 11 {
		t.Errorf("info gate exit = %d, want 11", code)
	}
}

func TestAHSystematicReviewPresetUnchanged(t *testing.T) {
	want := []string{"duplicate_candidates", "missing_abstract", "missing_pdf", "broken_attachment_file"}
	if got := healthPresets["systematic-review"]; !slices.Equal(got, want) {
		t.Fatalf("systematic-review = %v, want %v", got, want)
	}
	items := append(ahPortabilityItems("linked_file"),
		ahAttachment("DUP1", "PI", "imported_file", "application/pdf", "abc", 10),
		ahAttachment("DUP2", "PL", "imported_file", "application/pdf", "abc", 10),
	)
	db := ahStore(t, items)
	report, err := assembleHealthReport(db, newHealthCtx("systematic-review", false), "systematic-review", healthPresets["systematic-review"], sevHigh, scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	for _, f := range report.Findings {
		if f.Kind == "nonportable_attachment" || f.Kind == duplicateAttachmentBytesKind {
			t.Errorf("systematic-review produced %s", f.Kind)
		}
	}
}

// ahRunHealth runs `library health` through Cobra over a synced temp store.
func ahRunHealth(t *testing.T, items []json.RawMessage, args ...string) (healthReport, int) {
	t.Helper()
	auditIsolateEnv(t, "")
	auditSeedDB(t, items)
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.SaveSyncState("items", "", len(items)); err != nil {
		t.Fatalf("sync state: %v", err)
	}
	_ = db.Close()

	cmd := newLibraryHealthCmd(&rootFlags{asJSON: true, timeout: 2 * time.Second})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	runErr := cmd.Execute()
	var report healthReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return report, ExitCode(runErr)
}

func TestAHAllFailOnHighExitUnaffectedByAttachmentFindings(t *testing.T) {
	clean, cleanCode := ahRunHealth(t, ahPortabilityItems("imported_file"), "--for", "all", "--fail-on", "high")
	dirtyItems := append(ahPortabilityItems("linked_file"),
		ahAttachment("DUP1", "PI", "imported_file", "application/pdf", "abc", 10),
		ahAttachment("DUP2", "PI", "imported_url", "application/pdf", "abc", 10),
	)
	dirty, dirtyCode := ahRunHealth(t, dirtyItems, "--for", "all", "--fail-on", "high")

	if len(ahFindings(dirty, "nonportable_attachment")) != 1 || len(ahFindings(dirty, duplicateAttachmentBytesKind)) != 1 {
		t.Fatalf("dirty findings = %+v, want one of each new kind", dirty.Findings)
	}
	if len(ahFindings(clean, "nonportable_attachment"))+len(ahFindings(clean, duplicateAttachmentBytesKind)) != 0 {
		t.Fatalf("clean twin reported attachment findings: %+v", clean.Findings)
	}
	if dirtyCode != cleanCode || dirty.Gate == nil || clean.Gate == nil || dirty.Gate.Status != clean.Gate.Status {
		t.Errorf("exit/gate dirty=%d %+v, clean=%d %+v; want identical", dirtyCode, dirty.Gate, cleanCode, clean.Gate)
	}
	if dirty.Summary.Critical != clean.Summary.Critical || dirty.Summary.High != clean.Summary.High {
		t.Errorf("summary dirty %+v vs clean %+v: new findings moved critical/high", dirty.Summary, clean.Summary)
	}
}

// ahDuplicateItems:
//   - md5 aaa: X1/A1 (100 B) + X1/A2 (size unknown) => same_parent, 100 reclaimable;
//     X3/LNK is a linked_file with the same md5 and must not join.
//   - md5 bbb: X2/B1 + X3/B2, no sizes => cross_parent, reclaimable unknown.
//   - md5 ccc: X1/C1 + X2/C2, 10 B each => cross_parent, 10 reclaimable (not 20).
//   - X3/E1 has an empty md5 and X3/S1 a unique one: neither groups.
func ahDuplicateItems() []json.RawMessage {
	return []json.RawMessage{
		ahParent("X1", "COLD"),
		ahParent("X2", "COLO"),
		ahParent("X3", "COLO"),
		ahAttachment("A1", "X1", "imported_file", "application/pdf", "aaa", 100),
		ahAttachment("A2", "X1", "imported_file", "application/pdf", "aaa", -1),
		ahAttachment("LNK", "X3", "linked_file", "application/pdf", "aaa", 100),
		ahAttachment("B1", "X2", "imported_url", "application/pdf", "bbb", -1),
		ahAttachment("B2", "X3", "imported_file", "application/pdf", "bbb", -1),
		ahAttachment("C1", "X1", "imported_file", "application/epub+zip", "ccc", 10),
		ahAttachment("C2", "X2", "imported_file", "application/epub+zip", "ccc", 10),
		ahAttachment("E1", "X3", "imported_file", "application/pdf", "", 50),
		ahAttachment("S1", "X3", "imported_file", "application/pdf", "zzz", 50),
	}
}

type ahDupGroup struct {
	MD5              string `json:"md5"`
	Class            string `json:"class"`
	Count            int    `json:"count"`
	ReclaimableBytes *int   `json:"reclaimable_bytes"`
	Attachments      []struct {
		Key string `json:"key"`
	} `json:"attachments"`
}

func ahRunDuplicateAudit(t *testing.T, args ...string) ([]ahDupGroup, []Finding) {
	t.Helper()
	out, _, err := runItemsAudit(t, &rootFlags{asJSON: true, timeout: 2 * time.Second}, append([]string{"--duplicate-attachment-bytes"}, args...)...)
	if err != nil {
		t.Fatalf("items audit: %v", err)
	}
	m := decodeAuditMap(t, out)
	var groups []ahDupGroup
	if err := json.Unmarshal(m[duplicateAttachmentBytesKind], &groups); err != nil {
		t.Fatalf("decode groups: %v", err)
	}
	return groups, decodeAuditFindings(t, m["findings"])
}

func TestAHItemsAuditDuplicateAttachmentBytes(t *testing.T) {
	auditIsolateEnv(t, "")
	auditSeedDB(t, ahDuplicateItems())

	groups, findings := ahRunDuplicateAudit(t)
	byMD5 := map[string]ahDupGroup{}
	for _, g := range groups {
		byMD5[g.MD5] = g
	}
	if len(groups) != 3 {
		t.Fatalf("groups = %+v, want aaa, bbb, ccc", groups)
	}
	aaa := byMD5["aaa"]
	if aaa.Class != "same_parent" || aaa.Count != 2 || aaa.ReclaimableBytes == nil || *aaa.ReclaimableBytes != 100 {
		t.Errorf("aaa = %+v, want same_parent x2 (linked file excluded), 100 reclaimable", aaa)
	}
	for _, a := range aaa.Attachments {
		if a.Key == "LNK" {
			t.Errorf("linked file joined a stored-bytes group")
		}
	}
	if bbb := byMD5["bbb"]; bbb.Class != "cross_parent" || bbb.ReclaimableBytes != nil {
		t.Errorf("bbb = %+v, want cross_parent with unknown reclaimable bytes", bbb)
	}
	if ccc := byMD5["ccc"]; ccc.Class != "cross_parent" || ccc.ReclaimableBytes == nil || *ccc.ReclaimableBytes != 10 {
		t.Errorf("ccc = %+v, want cross_parent, 10 reclaimable (one copy counted once)", ccc)
	}
	if len(findings) != 3 {
		t.Fatalf("findings = %+v, want one per group", findings)
	}
	for _, f := range findings {
		if f.Kind != duplicateAttachmentBytesKind || f.Autofixable || f.RecommendedAction == nil || f.RecommendedAction.Command != "" {
			t.Errorf("finding %+v must be review-only", f)
		}
	}

	// Scope COLD keeps only X1: aaa survives; ccc loses X2's copy and with it
	// the group; bbb has no member in scope.
	scoped, _ := ahRunDuplicateAudit(t, "--scope", "collection:COLD")
	if len(scoped) != 1 || scoped[0].MD5 != "aaa" {
		t.Fatalf("scoped groups = %+v, want only aaa", scoped)
	}
}

func TestAHHealthDuplicateAttachmentBytesScope(t *testing.T) {
	db := ahStore(t, ahDuplicateItems())
	kinds := []string{duplicateAttachmentBytesKind}

	all, err := assembleHealthReport(db, newHealthCtx("all", false), "all", kinds, "", scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if n := len(ahFindings(all, duplicateAttachmentBytesKind)); n != 3 {
		t.Fatalf("library findings = %d, want 3", n)
	}

	// X2 and X3 only: bbb (X2+X3) stays; ccc drops X1's copy and dissolves.
	scoped, err := assembleHealthReport(db, newHealthCtx("all", false), "all", kinds, "", scopeResult{Keys: []string{"X2", "X3"}, Expr: "collection:COLO"})
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	got := ahFindings(scoped, duplicateAttachmentBytesKind)
	if len(got) != 1 || got[0].Evidence["value"] != "bbb" {
		t.Fatalf("scoped findings = %+v, want only bbb", got)
	}
	if _, ok := got[0].Evidence["reclaimable_bytes"]; ok {
		t.Errorf("unknown size reported reclaimable bytes: %v", got[0].Evidence)
	}
}
