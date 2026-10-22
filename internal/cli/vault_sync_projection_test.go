// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

// seedVaultProjectionStore seeds four notes whose tags, venue, and relations
// exercise the projection: A relates to B (and B back to A, which must not
// duplicate), to a key outside the run, and receives an incoming edge from C.
// A non-dc:relation predicate toward C and D's publisher must project nothing.
func seedVaultProjectionStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"AAAA1111","version":1,"data":{"key":"AAAA1111","itemType":"journalArticle","title":"Alpha","citationKey":"alpha2020","publicationTitle":"Journal of Tests","bookTitle":"ignored",
			"tags":[{"tag":"zeta"},{"tag":"alpha","type":1},{"tag":"zeta","type":1},{"tag":"2020"},{"tag":"yes: maybe"}],
			"relations":{"dc:relation":["http://zotero.org/users/1/items/BBBB2222","http://zotero.org/users/1/items/OUTS9999"],"owl:sameAs":"http://zotero.org/users/1/items/DDDD4444"}}}`),
		json.RawMessage(`{"key":"BBBB2222","version":1,"data":{"key":"BBBB2222","itemType":"bookSection","title":"Beta","citationKey":"beta2021","bookTitle":"Book Title",
			"relations":{"dc:relation":"http://zotero.org/users/1/items/AAAA1111"}}}`),
		json.RawMessage(`{"key":"CCCC3333","version":1,"data":{"key":"CCCC3333","itemType":"conferencePaper","title":"Gamma","citationKey":"gamma2022","proceedingsTitle":"Proc",
			"relations":{"dc:relation":"http://zotero.org/users/1/items/AAAA1111"}}}`),
		json.RawMessage(`{"key":"DDDD4444","version":1,"data":{"key":"DDDD4444","itemType":"book","title":"Delta","citationKey":"delta2023","publisher":"Pub"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()
}

func vaultSyncCounts(t *testing.T, out string) map[string]int {
	t.Helper()
	var report struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out)
	}
	return report.Counts
}

func TestVaultSyncProjectsTagsVenueAndRelatedIntoObsidianNotes(t *testing.T) {
	seedVaultProjectionStore(t)
	vault := t.TempDir()
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, allowDestructive: true}
	runVaultSync(t, flags, []string{"--vault", vault})

	alpha := readNote(t, filepath.Join(vault, "alpha2020.md"))
	for _, want := range []string{
		"zotero_tags:\n  - \"2020\"\n  - alpha\n  - \"yes: maybe\"\n  - zeta\n",
		"venue: Journal of Tests\n",
		"related:\n  - \"[[beta2021]]\"\n  - \"[[gamma2022]]\"\n  - \"zotero://select/library/items/OUTS9999\"\n",
	} {
		if !strings.Contains(alpha, want) {
			t.Errorf("alpha note missing %q:\n%s", want, alpha)
		}
	}
	if strings.Contains(alpha, "ignored") {
		t.Errorf("venue must prefer publicationTitle over bookTitle:\n%s", alpha)
	}
	// The user-facing tags key is not managed.
	if strings.Contains(alpha, "\ntags:") {
		t.Errorf("sync must not own the tags key:\n%s", alpha)
	}

	beta := readNote(t, filepath.Join(vault, "beta2021.md"))
	for _, want := range []string{"venue: Book Title\n", "related:\n  - \"[[alpha2020]]\"\n", "zotero_tags: []\n"} {
		if !strings.Contains(beta, want) {
			t.Errorf("beta note missing %q:\n%s", want, beta)
		}
	}
	gamma := readNote(t, filepath.Join(vault, "gamma2022.md"))
	for _, want := range []string{"venue: Proc\n", "related:\n  - \"[[alpha2020]]\"\n"} {
		if !strings.Contains(gamma, want) {
			t.Errorf("gamma note missing %q (incoming edge only):\n%s", want, gamma)
		}
	}
	delta := readNote(t, filepath.Join(vault, "delta2023.md"))
	if strings.Contains(delta, "venue:") {
		t.Errorf("a publisher is not a venue; delta must omit the key:\n%s", delta)
	}
	if !strings.Contains(delta, "related: []\n") {
		t.Errorf("delta note missing empty related list:\n%s", delta)
	}
}

func TestVaultSyncProjectionKeepsUserKeysAndSecondSyncIsUnchanged(t *testing.T) {
	seedVaultProjectionStore(t)
	vault := t.TempDir()
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, allowDestructive: true}
	runVaultSync(t, flags, []string{"--vault", vault})

	alphaPath := filepath.Join(vault, "alpha2020.md")
	deltaPath := filepath.Join(vault, "delta2023.md")
	writeFile(t, alphaPath, strings.Replace(readNote(t, alphaPath), "\nzotero_key:", "\ntags:\n  - mine\ncustom: keep\nzotero_key:", 1))
	// A venue line on a note whose item has no venue is stale managed state.
	writeFile(t, deltaPath, strings.Replace(readNote(t, deltaPath), "\nzotero_key:", "\nvenue: Stale\nzotero_key:", 1))

	counts := vaultSyncCounts(t, runVaultSync(t, flags, []string{"--vault", vault}))
	if counts["updated"] != 1 || counts["unchanged"] != 3 {
		t.Fatalf("sync after user edits = %+v, want only the stale-venue note updated", counts)
	}
	alpha := readNote(t, alphaPath)
	if !strings.Contains(alpha, "\ntags:\n  - mine\ncustom: keep\n") {
		t.Errorf("user keys not preserved:\n%s", alpha)
	}
	if strings.Contains(readNote(t, deltaPath), "venue:") {
		t.Errorf("stale venue survived the sync:\n%s", readNote(t, deltaPath))
	}

	counts = vaultSyncCounts(t, runVaultSync(t, flags, []string{"--vault", vault}))
	if counts["unchanged"] != 4 || counts["updated"] != 0 || counts["created"] != 0 {
		t.Fatalf("third sync = %+v, want everything unchanged", counts)
	}
}

func TestVaultSyncProjectsTagsVenueAndRelatedIntoLogseqNotes(t *testing.T) {
	seedVaultProjectionStore(t)
	vault := t.TempDir()
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, allowDestructive: true}
	args := []string{"--vault", vault, "--format", "logseq"}
	runVaultSync(t, flags, args)

	alphaPath := filepath.Join(vault, "alpha2020.md")
	alpha := readNote(t, alphaPath)
	for _, want := range []string{
		"zotero-tags:: 2020, alpha, yes: maybe, zeta\n",
		"venue:: Journal of Tests\n",
		"related:: [[beta2021]], [[gamma2022]], zotero://select/library/items/OUTS9999\n",
	} {
		if !strings.Contains(alpha, want) {
			t.Errorf("alpha note missing %q:\n%s", want, alpha)
		}
	}
	delta := readNote(t, filepath.Join(vault, "delta2023.md"))
	if strings.Contains(delta, "venue::") {
		t.Errorf("delta must omit venue:\n%s", delta)
	}

	writeFile(t, alphaPath, strings.Replace(alpha, "zotero-key::", "tags:: mine\nzotero-key::", 1))
	counts := vaultSyncCounts(t, runVaultSync(t, flags, args))
	if counts["unchanged"] != 4 {
		t.Fatalf("second Logseq sync = %+v, want everything unchanged", counts)
	}
	if !strings.Contains(readNote(t, alphaPath), "tags:: mine\n") {
		t.Errorf("user tags property not preserved:\n%s", readNote(t, alphaPath))
	}
}

// A neighbour whose note name cannot sit inside [[...]] falls back to the
// select link, and a neighbour outside the run is never wikilinked.
func TestVaultRelatedLinks(t *testing.T) {
	got := vaultRelatedLinks("SELF",
		map[string]bool{"SELF": true, "IN": true, "BAD": true, "OUT": true, "": true},
		map[string]string{"SELF": "self.md", "IN": "in.md", "BAD": "a#b.md"})
	want := []string{
		"[[in]]",
		"zotero://select/library/items/BAD",
		"zotero://select/library/items/OUT",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("vaultRelatedLinks = %q, want %q", got, want)
	}
}

func TestVaultTagNamesAndStrictScalars(t *testing.T) {
	tags := vaultTagNames([]any{
		map[string]any{"tag": "b"}, map[string]any{"tag": "a", "type": float64(1)},
		map[string]any{"tag": "b", "type": float64(1)}, map[string]any{"tag": "  "}, "c",
	})
	if strings.Join(tags, ",") != "a,b,c" {
		t.Fatalf("vaultTagNames = %q, want a,b,c", tags)
	}
	for in, want := range map[string]string{
		"plain":   "plain",
		"2020":    `"2020"`,
		"true":    `"true"`,
		"No":      `"No"`,
		"null":    `"null"`,
		"-dash":   `"-dash"`,
		"a:b":     `"a:b"`,
		"tab\tx":  `"tab\tx"`,
		"1e3":     `"1e3"`,
		"~":       `"~"`,
		"x y":     "x y",
		"é-ok":    "é-ok",
		"nullish": "nullish",
	} {
		if got := yamlStrictScalar(in); got != want {
			t.Errorf("yamlStrictScalar(%q) = %s, want %s", in, got, want)
		}
	}
}
