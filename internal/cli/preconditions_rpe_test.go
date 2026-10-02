// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

func rpeIsolateSelection(t *testing.T) {
	t.Helper()
	t.Setenv("ZOTERO_PROFILE", "")
	t.Setenv("ZOTERO_GROUP", "")
}

func rpeSeedMirror(t *testing.T, seed func(*store.Store)) {
	t.Helper()
	path := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenWithContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed(db)
}

func TestRpeCompletedEmptyItemsMirror(t *testing.T) {
	for _, capability := range []string{"import scan", "import resolve", "items duplicates resolve"} {
		t.Run(capability, func(t *testing.T) {
			root, _, out, _ := newPreflightTestRoot(t)
			rpeIsolateSelection(t)
			rpeSeedMirror(t, func(db *store.Store) {
				if err := db.SaveSyncState("items", "", 0); err != nil {
					t.Fatal(err)
				}
				counts, err := db.Status()
				if err != nil {
					t.Fatal(err)
				}
				for resource, count := range counts {
					if count != 0 {
						t.Fatalf("%s has %d rows; fixture must be truly empty", resource, count)
					}
				}
			})
			args := append([]string{"--json"}, strings.Fields(capability)...)
			if strings.HasPrefix(capability, "import ") {
				args = append(args, t.TempDir())
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("%s: %v; output=%q", capability, err, out.String())
			}
			switch capability {
			case "import scan":
				var report scanReport
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				if len(report.Results) != 0 {
					t.Fatalf("results = %+v, want empty scan", report.Results)
				}
			case "import resolve":
				var manifest importManifest
				if err := json.Unmarshal(out.Bytes(), &manifest); err != nil {
					t.Fatal(err)
				}
				if manifest.SchemaVersion != importManifestSchemaVersion || len(manifest.Entries) != 0 {
					t.Fatalf("manifest = %+v, want an empty valid manifest", manifest)
				}
			case "items duplicates resolve":
				var env mutation.Envelope
				if err := json.Unmarshal(out.Bytes(), &env); err != nil {
					t.Fatal(err)
				}
				if !env.OK || env.Operation != "items.duplicates.resolve" || len(env.Plan.Operations) != 0 || env.Plan.Summary.Planned != 0 {
					t.Fatalf("envelope = %+v, want no merge operations", env)
				}
			}
		})
	}
}

func TestRpeItemsMirrorRefusesAbsentOrUnfinishedPass(t *testing.T) {
	for _, capability := range []string{"import scan", "import resolve", "items duplicates resolve"} {
		for _, state := range []string{"absent", "unfinished empty", "unfinished with rows"} {
			t.Run(capability+"/"+state, func(t *testing.T) {
				root, _, out, _ := newPreflightTestRoot(t)
				rpeIsolateSelection(t)
				if state != "absent" {
					rpeSeedMirror(t, func(db *store.Store) {
						if state == "unfinished with rows" {
							item := json.RawMessage(`{"key":"RPEITEM1","data":{"key":"RPEITEM1","itemType":"book","title":"Incomplete mirror"}}`)
							if _, _, err := db.UpsertBatchContext(context.Background(), "items", []json.RawMessage{item}); err != nil {
								t.Fatal(err)
							}
						}
						if err := db.SaveSyncResumeStateContext(context.Background(), "items", "100", "items?limit=100", 1); err != nil {
							t.Fatal(err)
						}
					})
				}
				args := append([]string{"--json"}, strings.Fields(capability)...)
				if strings.HasPrefix(capability, "import ") {
					args = append(args, t.TempDir())
				}
				root.SetArgs(args)
				err := root.Execute()
				if ExitCode(err) != 9 {
					t.Fatalf("exit = %d (%v), want 9; output=%q", ExitCode(err), err, out.String())
				}
				var env preconditionUnmetEnvelope
				if err := json.Unmarshal(out.Bytes(), &env); err != nil {
					t.Fatal(err)
				}
				if env.Kind != "precondition_unmet" || env.Precondition != preconditionSyncedStore || env.Capability != capability {
					t.Fatalf("unexpected refusal: %+v", env)
				}
			})
		}
	}
}

func TestRpeSummarizeProfileCohort(t *testing.T) {
	for _, selection := range []string{"flag", "environment"} {
		for _, cohortFlag := range []string{"collection", "scope"} {
			t.Run(selection+"/"+cohortFlag, func(t *testing.T) {
				root, _, out, _ := newPreflightTestRoot(t)
				rpeIsolateSelection(t)
				rpeSeedMirror(t, func(db *store.Store) {
					collection := json.RawMessage(`{"key":"RPECOLL1","data":{"key":"RPECOLL1","name":"Profile shelf"}}`)
					item := json.RawMessage(`{"key":"RPEITEM1","data":{"key":"RPEITEM1","itemType":"book","title":"Profile item","collections":["RPECOLL1"]}}`)
					for resource, rows := range map[string][]json.RawMessage{"collections": {collection}, "items": {item}} {
						if _, _, err := db.UpsertBatchContext(context.Background(), resource, rows); err != nil {
							t.Fatal(err)
						}
						if err := db.SaveSyncState(resource, "", 1); err != nil {
							t.Fatal(err)
						}
					}
				})
				value := "RPECOLL1"
				if cohortFlag == "scope" {
					value = "collection:" + value
				}
				profile := Profile{Name: "rpe-cohort", Values: map[string]string{cohortFlag: value}}
				if err := saveProfileStore(&profileStore{Profiles: map[string]Profile{profile.Name: profile}}); err != nil {
					t.Fatal(err)
				}
				args := []string{"--json", "items", "summarize", "--no-fulltext"}
				if selection == "flag" {
					args = append(args, "--profile", profile.Name)
				} else {
					t.Setenv("ZOTERO_PROFILE", profile.Name)
				}
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatalf("summarize profile: %v; output=%q", err, out.String())
				}
				var bundle summarizeCollectionBundle
				if err := json.Unmarshal(out.Bytes(), &bundle); err != nil {
					t.Fatal(err)
				}
				if bundle.Collection != "RPECOLL1" || bundle.ItemCount != 1 || len(bundle.Items) != 1 || bundle.Items[0].Key != "RPEITEM1" {
					t.Fatalf("bundle = %+v, want the profile collection and its item", bundle)
				}
			})
		}
	}
}

func TestRpeSummarizeMissingInputRemainsUsageError(t *testing.T) {
	root, _, out, _ := newPreflightTestRoot(t)
	rpeIsolateSelection(t)
	root.SetArgs([]string{"--json", "items", "summarize"})
	err := root.Execute()
	want := "missing <itemKey>: pass an item key, --collection <key>, or --scope <spec>"
	if ExitCode(err) != 2 || err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v (exit %d), want usage error %q", err, ExitCode(err), want)
	}
	if !strings.Contains(err.Error(), "(usage: zotio items summarize") {
		t.Fatalf("error = %q, want the command use line", err.Error())
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want no store refusal", out.String())
	}
}
