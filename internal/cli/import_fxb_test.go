// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// import scan and import resolve <dir> classify DOIs against the items
// mirror only. The shared synced_store precondition accepts a checkpoint for
// any resource, so a collections-only sync used to pass it and every DOI then
// classified as new, which import apply would turn into duplicates.

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

// fxbSeedMirror writes a store holding one synced collection, then lets the
// caller record whatever items state the case needs.
func fxbSeedMirror(t *testing.T, items func(*store.Store) error) {
	t.Helper()
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	collection := json.RawMessage(`{"key":"FXBCOL01","data":{"key":"FXBCOL01","name":"Shelf"}}`)
	if _, _, err := db.UpsertBatchContext(context.Background(), "collections", []json.RawMessage{collection}); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if err := db.SaveSyncState("collections", "", 1); err != nil {
		t.Fatalf("save collections sync: %v", err)
	}
	if items != nil {
		if err := items(db); err != nil {
			t.Fatalf("seed items state: %v", err)
		}
	}
}

// fxbSeedHeldItem stores an item whose DOI matches the staged PDF, so a mirror
// that classifies it as new has lost track of an item it holds.
func fxbSeedHeldItem(db *store.Store) error {
	item := json.RawMessage(`{"key":"FXBHELD1","data":{"key":"FXBHELD1","itemType":"journalArticle","title":"Held","DOI":"10.1234/demo"}}`)
	_, _, err := db.UpsertBatchContext(context.Background(), "items", []json.RawMessage{item})
	return err
}

func TestFxbImportClassificationRequiresCompletedItemsSync(t *testing.T) {
	refusals := []struct {
		name string
		seed func(*store.Store) error
	}{
		{name: "collections only", seed: nil},
		{name: "unfinished initial items pass", seed: func(db *store.Store) error {
			if err := fxbSeedHeldItem(db); err != nil {
				return err
			}
			return db.SaveSyncResumeStateContext(context.Background(), "items", "100", "items?limit=100", 1)
		}},
	}
	for _, capability := range []string{"import scan", "import resolve"} {
		for _, tc := range refusals {
			t.Run(capability+"/"+tc.name, func(t *testing.T) {
				root, _, out, _ := newPreflightTestRoot(t)
				fxbSeedMirror(t, tc.seed)
				root.SetArgs(append([]string{"--json", "--timeout", "5s"}, append(strings.Fields(capability), iprWriteDOIPDF(t))...))
				err := root.Execute()

				iprAssertSyncedStoreRefusal(t, err, out.Bytes(), capability)
				env := iprDecodeSoleEnvelope(t, out.Bytes())
				if !strings.Contains(strings.Join(env.Remediation, "\n"), "zotio sync --resources items") {
					t.Fatalf("remediation = %q, want the items sync", env.Remediation)
				}
			})
		}
	}
}

func TestFxbImportClassificationAcceptsCompletedItemsMirror(t *testing.T) {
	cases := []struct {
		name       string
		seed       func(*store.Store) error
		wantStatus string
	}{
		{name: "completed empty items sync", wantStatus: "new", seed: func(db *store.Store) error {
			return db.SaveSyncState("items", "", 0)
		}},
		// A resume cursor beside a library-version stamp is an interrupted
		// incremental pass over a completed one: the held item, which has no
		// PDF yet, is mirrored and classifies as an attach candidate.
		{name: "interrupted incremental pass", wantStatus: "attach_candidate", seed: func(db *store.Store) error {
			if err := fxbSeedHeldItem(db); err != nil {
				return err
			}
			if err := db.SaveLibraryVersion("items", "http://localhost:23119/api/users/0", 7); err != nil {
				return err
			}
			return db.SaveSyncResumeStateContext(context.Background(), "items", "100", "items?since=7", 1)
		}},
	}
	for _, tc := range cases {
		t.Run("import scan/"+tc.name, func(t *testing.T) {
			root, _, out, _ := newPreflightTestRoot(t)
			fxbSeedMirror(t, tc.seed)
			root.SetArgs([]string{"--json", "import", "scan", iprWriteDOIPDF(t)})
			if err := root.Execute(); err != nil {
				t.Fatalf("import scan: %v; output=%q", err, out.String())
			}
			var report scanReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("decode scan report %q: %v", out.String(), err)
			}
			if len(report.Results) != 1 || report.Results[0].Status != tc.wantStatus {
				t.Fatalf("results = %+v, want one %s", report.Results, tc.wantStatus)
			}
		})
		t.Run("import resolve/"+tc.name, func(t *testing.T) {
			root, _, out, _ := newPreflightTestRoot(t)
			srv := importResolveCrossRefWorkServer(t, "Resolved", "10.1234/demo")
			withBase(t, &enrichCrossRefBase, srv.URL)
			fxbSeedMirror(t, tc.seed)
			root.SetArgs([]string{"--json", "--timeout", "5s", "import", "resolve", iprWriteDOIPDF(t)})
			if err := root.Execute(); err != nil {
				t.Fatalf("import resolve: %v; output=%q", err, out.String())
			}
			var got importManifest
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("decode manifest %q: %v", out.String(), err)
			}
			if len(got.Entries) != 1 || got.Entries[0].Classification != tc.wantStatus {
				t.Fatalf("entries = %+v, want one %s", got.Entries, tc.wantStatus)
			}
		})
	}
}
