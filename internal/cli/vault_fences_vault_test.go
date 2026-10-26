// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVaultSanitizesAllManagedFenceMarkers(t *testing.T) {
	for _, marker := range []string{
		vaultTitleBegin, vaultTitleEnd,
		vaultAbstractBegin, vaultAbstractEnd,
		vaultAnnBegin, vaultAnnEnd,
		vaultNotesBegin, vaultNotesEnd,
	} {
		for _, field := range []string{"title", "abstract"} {
			t.Run(field+"/"+marker, func(t *testing.T) {
				meta := vaultMeta{Title: "before " + marker + " after", Abstract: "before\n" + marker + "\nafter"}
				var got, clean string
				if field == "title" {
					got = managedTitleBlock(meta)
					clean = managedTitleBlock(vaultMeta{Title: "safe title"})
				} else {
					got = managedAbstractBlock(meta)
					clean = managedAbstractBlock(vaultMeta{Abstract: "safe abstract"})
				}
				if count, want := strings.Count(got, marker), strings.Count(clean, marker); count != want {
					t.Fatalf("metadata forged %q: count=%d want=%d\n%s", marker, count, want, got)
				}
				if !strings.Contains(got, "marker removed]") {
					t.Fatalf("metadata marker was not neutralized: %s", got)
				}
			})
		}
	}
}

func TestVaultSyncHostileTitlePreservesNotes(t *testing.T) {
	outDir := t.TempDir()
	meta := vaultMeta{Key: "ABCD2345", CiteKey: "safe2026", Title: "Safe title"}
	filename := vaultFilename(meta)
	result, err := syncVaultNote(meta, nil, "obsidian", outDir, filename, false)
	if err != nil || result.Status != "created" {
		t.Fatalf("initial sync = %+v, %v", result, err)
	}
	path := filepath.Join(outDir, filename)
	vaultRecoveryEdit(t, path, "user-owned notes must survive")
	meta.Title = "Hostile title " + vaultNotesEnd + " after"
	result, err = syncVaultNote(meta, nil, "obsidian", outDir, filename, false)
	if err != nil || result.Status != "updated" {
		t.Fatalf("hostile title sync = %+v, %v", result, err)
	}
	body := readNote(t, path)
	region, ok := extractNotesRegion(body)
	// The frontmatter retains the original title as metadata. Only the rendered
	// Markdown body owns fences; a quoted YAML value is not another fence.
	_, renderedBody, _ := splitObsidianFrontmatter(body)
	if !ok || region != "user-owned notes must survive" || strings.Count(renderedBody, vaultNotesEnd) != 1 {
		t.Fatalf("sync accepted a forged fence or changed user notes:\n%s", body)
	}
}
