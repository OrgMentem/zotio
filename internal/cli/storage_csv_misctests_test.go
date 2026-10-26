// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"zotio/internal/zoteroprefs"
)

func TestStoredUploadGroupSyncOffMiscTests(t *testing.T) {
	dir := t.TempDir()
	writeTestPrefs(t, dir, `user_pref("extensions.zotero.sync.storage.groups.enabled", false);`)
	fs, err := zoteroprefs.LoadProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	stubLoadedZoteroFileStorage(t, fs)
	got := storedUploadDecision(t.Context(), &rootFlags{group: "123", maxChanges: -1}, storedUploadToExistingItem)
	if got.reason != storedUploadReasonGroupSyncOff || !slices.Equal(got.profiles, []string{dir}) {
		t.Fatalf("decision = %+v, want group-sync-off naming %s", got, dir)
	}
}

func TestStoredUploadPartiallyUnreadableMiscTests(t *testing.T) {
	cloud, broken := t.TempDir(), t.TempDir()
	writeTestPrefs(t, cloud, `user_pref("extensions.zotero.sync.storage.protocol", "zotero");`)
	if err := os.Mkdir(filepath.Join(broken, "prefs.js"), 0o750); err != nil {
		t.Fatal(err)
	}
	fs, err := zoteroprefs.LoadAcrossForTest([]string{cloud, broken}, cloud)
	if err != nil {
		t.Fatal(err)
	}
	stubLoadedZoteroFileStorage(t, fs)
	got := storedUploadDecision(t.Context(), &rootFlags{maxChanges: -1}, storedUploadToExistingItem)
	if got.reason != storedUploadReasonUnreadable || !slices.Equal(got.profiles, []string{broken}) {
		t.Fatalf("decision = %+v, want unreadable naming %s", got, broken)
	}
}

func TestPrintCSVNeutralisesFormulaCellsMiscTests(t *testing.T) {
	for _, input := range []string{"=1+1", "+1", "-2", "@SUM(A1)", "\tx", "\rx", "plain"} {
		t.Run(input, func(t *testing.T) {
			want := input
			if input != "plain" && input != "" {
				want = "'" + input
			}
			var row bytes.Buffer
			if err := writeTabularRow(&row, []string{input}, true); err != nil {
				t.Fatal(err)
			}
			record, err := csv.NewReader(&row).Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(record) != 1 || record[0] != want {
				t.Fatalf("CSV row = %q, want %q", record, want)
			}

			data, err := json.Marshal([]map[string]string{{"title": input}})
			if err != nil {
				t.Fatal(err)
			}
			var output, plain bytes.Buffer
			if err := printCSV(&output, data, false); err != nil {
				t.Fatal(err)
			}
			records, err := csv.NewReader(&output).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			// The shared cell renderer collapses TAB and CR before CSV quoting.
			cell := strings.NewReplacer("\t", " ", "\r", " ").Replace(input)
			csvWant := cell
			if strings.HasPrefix(input, "=") || strings.HasPrefix(input, "+") || strings.HasPrefix(input, "-") || strings.HasPrefix(input, "@") {
				csvWant = "'" + cell
			}
			if len(records) != 2 || len(records[1]) != 1 || records[1][0] != csvWant {
				t.Fatalf("CSV records = %q, want cell %q", records, csvWant)
			}
			if err := printPlain(&plain, data, false); err != nil {
				t.Fatal(err)
			}
			if plain.String() != "title\n"+cell+"\n" {
				t.Fatalf("plain = %q, want unprefixed cell %q", plain.String(), cell)
			}
		})
	}
}
