// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 49r4): cover vault note creation, idempotent + non-clobbering
// merge, backlinks, dry-run, and Logseq output.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotero-pp-cli/internal/store"
)

func seedVaultStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.Open(defaultDBPath("zotero-pp-cli"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","itemType":"journalArticle","title":"Attention Is All You Need","citationKey":"vaswani2017","abstractNote":"We propose the Transformer.","date":"2017-06-12","DOI":"10.5555/attn","url":"https://arxiv.org/abs/1706.03762","collections":["COL1"],"creators":[{"firstName":"Ashish","lastName":"Vaswani","creatorType":"author"}]}}`),
		json.RawMessage(`{"key":"ATT1","version":1,"data":{"key":"ATT1","itemType":"attachment","parentItem":"K1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","parentItem":"ATT1","annotationText":"the dominant models","annotationComment":"key claim","annotationPageLabel":"5"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()
}

func runVaultSync(t *testing.T, flags *rootFlags, args []string) string {
	t.Helper()
	cmd := newVaultSyncCmd(flags)
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("vault sync %v: %v", args, err)
	}
	return out.String()
}

func TestVaultSyncCreatesObsidianNote(t *testing.T) {
	seedVaultStore(t)
	vault := filepath.Join(t.TempDir(), "vault")

	out := runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})
	var report struct {
		Created int `json:"created"`
		Results []vaultSyncResult
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode report %q: %v", out, err)
	}
	if report.Created != 1 {
		t.Fatalf("created = %d, want 1 (attachment/annotation excluded)", report.Created)
	}

	body, err := os.ReadFile(filepath.Join(vault, "vaswani2017.md"))
	if err != nil {
		t.Fatalf("note not written: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"citekey: vaswani2017",
		"title: Attention Is All You Need",
		`zotero: "zotero://select/library/items/K1"`,
		"the dominant models",
		"zotero://open-pdf/library/items/ATT1?annotation=AN1",
		"p. 5",
		"Note: key claim",
		vaultAnnBegin,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("note missing %q\n---\n%s", want, s)
		}
	}
}

func TestVaultSyncIdempotent(t *testing.T) {
	seedVaultStore(t)
	vault := filepath.Join(t.TempDir(), "vault")

	runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})
	first, _ := os.ReadFile(filepath.Join(vault, "vaswani2017.md"))

	out := runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})
	var report struct {
		Created   int `json:"created"`
		Updated   int `json:"updated"`
		Unchanged int `json:"unchanged"`
	}
	_ = json.Unmarshal([]byte(out), &report)
	if report.Unchanged != 1 || report.Created != 0 || report.Updated != 0 {
		t.Errorf("second run = %+v, want unchanged=1", report)
	}
	second, _ := os.ReadFile(filepath.Join(vault, "vaswani2017.md"))
	if !bytes.Equal(first, second) {
		t.Errorf("note changed on idempotent re-run")
	}
}

func TestVaultSyncDryRunWritesNothing(t *testing.T) {
	seedVaultStore(t)
	vault := filepath.Join(t.TempDir(), "vault")

	out := runVaultSync(t, &rootFlags{asJSON: true, dryRun: true}, []string{"--out", vault})
	var report struct {
		Created int  `json:"created"`
		DryRun  bool `json:"dry_run"`
	}
	_ = json.Unmarshal([]byte(out), &report)
	if !report.DryRun || report.Created != 1 {
		t.Errorf("dry-run report = %+v, want dry_run + created=1", report)
	}
	if _, err := os.Stat(filepath.Join(vault, "vaswani2017.md")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote a file")
	}
}

// TestMergeObsidianPreservesUserContent is the critical no-clobber test: a
// user's extra frontmatter keys and prose survive a sync; only managed keys and
// the fenced annotation block change.
func TestMergeObsidianPreservesUserContent(t *testing.T) {
	existing := strings.Join([]string{
		"---",
		"title: Old Title",
		"status: reading",
		"tags:",
		"  - mytag",
		"citekey: oldkey",
		"---",
		"",
		"# Old Title",
		"",
		"My own synthesis prose.",
		"",
		"## Notes",
		"",
		"Important user notes.",
		"",
	}, "\n")

	meta := vaultMeta{
		Key: "K1", CiteKey: "vaswani2017", Title: "New Title",
		Authors: []string{"Vaswani, Ashish"}, Year: "2017",
		ItemType: "journalArticle", DOI: "10.1", URL: "https://x", Collections: []string{"C1"},
	}
	annBlock := renderAnnotationBlock([]annotationSummary{
		{Key: "AN1", ParentItem: "ATT1", Text: "hl", Comment: "c", Page: "3"},
	})
	merged := mergeObsidianNote(existing, managedObsidianFrontmatter(meta), annBlock)

	// Managed keys updated.
	if !strings.Contains(merged, "title: New Title") || !strings.Contains(merged, "citekey: vaswani2017") {
		t.Errorf("managed keys not updated:\n%s", merged)
	}
	// User keys + prose preserved.
	for _, want := range []string{"status: reading", "  - mytag", "My own synthesis prose.", "Important user notes."} {
		if !strings.Contains(merged, want) {
			t.Errorf("clobbered user content %q:\n%s", want, merged)
		}
	}
	// Annotation block materialized with backlink.
	if !strings.Contains(merged, vaultAnnBegin) || !strings.Contains(merged, "zotero://open-pdf/library/items/ATT1?annotation=AN1") {
		t.Errorf("annotation block/backlink missing:\n%s", merged)
	}
	// Old title value gone from frontmatter (replaced, not duplicated).
	if strings.Contains(merged, "title: Old Title") {
		t.Errorf("old managed value not replaced:\n%s", merged)
	}
}

func TestMergeObsidianReplacesExistingBlock(t *testing.T) {
	existing := strings.Join([]string{
		"---", "title: T", "---", "",
		"## Annotations", "",
		vaultAnnBegin, "- stale annotation", vaultAnnEnd,
		"", "## Notes", "kept",
	}, "\n")
	annBlock := renderAnnotationBlock([]annotationSummary{{Key: "A2", ParentItem: "P2", Text: "fresh", Page: "9"}})
	merged := mergeObsidianNote(existing, managedObsidianFrontmatter(vaultMeta{Key: "K", Title: "T"}), annBlock)
	if strings.Contains(merged, "stale annotation") {
		t.Errorf("stale annotation not replaced:\n%s", merged)
	}
	if !strings.Contains(merged, "fresh") || !strings.Contains(merged, "kept") {
		t.Errorf("fresh annotation or user content missing:\n%s", merged)
	}
	if strings.Count(merged, vaultAnnBegin) != 1 {
		t.Errorf("annotation block duplicated:\n%s", merged)
	}
}

func TestVaultSyncLogseqCreate(t *testing.T) {
	seedVaultStore(t)
	vault := filepath.Join(t.TempDir(), "vault")
	runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault, "--format", "logseq"})

	body, err := os.ReadFile(filepath.Join(vault, "vaswani2017.md"))
	if err != nil {
		t.Fatalf("logseq note not written: %v", err)
	}
	s := string(body)
	for _, want := range []string{"citekey:: vaswani2017", "zotero:: zotero://select/library/items/K1", vaultAnnBegin} {
		if !strings.Contains(s, want) {
			t.Errorf("logseq note missing %q\n%s", want, s)
		}
	}
}

func TestSanitizeVaultFilename(t *testing.T) {
	cases := map[string]string{
		"vaswani2017": "vaswani2017",
		"a/b:c*d?":    "a-b-c-d",
		"  spaced  ":  "spaced",
		"":            "untitled",
		"...":         "untitled",
	}
	for in, want := range cases {
		if got := sanitizeVaultFilename(in); got != want {
			t.Errorf("sanitizeVaultFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVaultSyncRequiresOut(t *testing.T) {
	seedVaultStore(t)
	cmd := newVaultSyncCmd(&rootFlags{})
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when --out is missing")
	}
}
