// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// import scan and import resolve <dir> classify PDFs against the synced
// mirror. Without one, every DOI reads as new: scan used to exit 0 with a bare
// sentence, and resolve built a manifest of creates that import apply would
// turn into duplicates of the whole library. Both now refuse with the shared
// synced_store precondition (exit 9), while a manifest refresh, which needs
// no mirror, still runs.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"zotio/internal/store"
)

// iprSeedSyncedLibrary isolates the environment and seeds a synced mirror.
func iprSeedSyncedLibrary(t *testing.T) {
	t.Helper()
	isolateDemoEnv(t, "0")
	iprSeedDemoStore(t)
}

// iprSeedDemoStore seeds the demo library with sync checkpoints into the
// current, already isolated environment. Its DOIs do not overlap the 10.1234
// and 10.5555 fixtures the resolve tests classify.
func iprSeedDemoStore(t *testing.T) {
	t.Helper()
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	if _, err := seedDemoStore(t.Context(), dbPath); err != nil {
		t.Fatalf("seed synced library: %v", err)
	}
}

// iprSeedUnsyncedStore writes a store that holds an item but never recorded a
// completed sync, the state an interrupted first sync leaves behind.
func iprSeedUnsyncedStore(t *testing.T) {
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
	item := json.RawMessage(`{"key":"IPRUNSY1","data":{"key":"IPRUNSY1","itemType":"journalArticle","title":"Half synced","DOI":"10.9999/ipr-unsynced"}}`)
	if _, _, err := db.UpsertBatchContext(context.Background(), "items", []json.RawMessage{item}); err != nil {
		t.Fatalf("seed unsynced item: %v", err)
	}
}

// iprWriteDOIPDF stages a PDF whose filename carries a DOI, so classification
// has something to call new or duplicate.
func iprWriteDOIPDF(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "10.1234%2Fdemo.pdf"), nil, 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	return dir
}

func iprDecodeSoleEnvelope(t *testing.T, out []byte) preconditionUnmetEnvelope {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(out))
	var env preconditionUnmetEnvelope
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("decode precondition envelope: %v; output=%q", err, out)
	}
	if dec.More() {
		t.Fatalf("stdout carries more than the refusal envelope: %q", out)
	}
	return env
}

func iprAssertSyncedStoreRefusal(t *testing.T, err error, out []byte, capability string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded without a synced mirror; output=%q", capability, out)
	}
	assertPreconditionExitCode(t, err, 9)
	env := iprDecodeSoleEnvelope(t, out)
	if env.Kind != "precondition_unmet" || env.Capability != capability || env.Precondition != preconditionSyncedStore {
		t.Fatalf("envelope = %+v, want precondition_unmet/%s/%s", env, capability, preconditionSyncedStore)
	}
	if env.Detail == "" || len(env.Remediation) == 0 {
		t.Fatalf("envelope = %+v, want a detail and remediation", env)
	}
}

func TestIprMirrorDependentImportsRefuseWithoutSyncedStore(t *testing.T) {
	stores := []struct {
		name string
		seed func(t *testing.T)
	}{
		{name: "absent store", seed: func(*testing.T) {}},
		{name: "never synced store", seed: iprSeedUnsyncedStore},
	}
	commands := []struct {
		capability string
		args       []string
	}{
		{capability: "import scan", args: []string{"import", "scan"}},
		{capability: "import resolve", args: []string{"import", "resolve"}},
	}
	for _, command := range commands {
		for _, st := range stores {
			t.Run(command.capability+"/"+st.name, func(t *testing.T) {
				root, _, out, _ := newPreflightTestRoot(t)
				st.seed(t)
				var providerHits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					providerHits.Add(1)
					http.Error(w, "unexpected provider request", http.StatusTeapot)
				}))
				t.Cleanup(srv.Close)
				withBase(t, &enrichCrossRefBase, srv.URL)

				args := append([]string{"--json", "--timeout", "5s"}, command.args...)
				root.SetArgs(append(args, iprWriteDOIPDF(t)))
				err := root.Execute()

				iprAssertSyncedStoreRefusal(t, err, out.Bytes(), command.capability)
				if hits := providerHits.Load(); hits != 0 {
					t.Fatalf("provider requests = %d, want none before the mirror check", hits)
				}
			})
		}
	}
}

// A caller that builds the scan command directly bypasses central preflight;
// the command still must not report success without classifying anything.
func TestIprImportScanCommandRefusesMissingStoreWithoutPreflight(t *testing.T) {
	isolateDemoEnv(t, "0")
	dir := iprWriteDOIPDF(t)

	t.Run("json", func(t *testing.T) {
		cmd := newImportScanCmd(&rootFlags{asJSON: true})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{dir})
		iprAssertSyncedStoreRefusal(t, cmd.Execute(), out.Bytes(), "import scan")
	})
	t.Run("human", func(t *testing.T) {
		cmd := newImportScanCmd(&rootFlags{})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{dir})
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("import scan succeeded without a mirror; stdout=%q", out.String())
		}
		assertPreconditionExitCode(t, err, 9)
		if !strings.Contains(err.Error(), "zotio sync") {
			t.Fatalf("error = %q, want the sync remediation", err)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout = %q, want nothing beside the refusal", out.String())
		}
	})
}

// A corrupt store is an open failure, never a missing store.
func TestIprImportScanCorruptStoreIsNotReportedMissing(t *testing.T) {
	isolateDemoEnv(t, "0")
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("not a SQLite database"), 0o600); err != nil {
		t.Fatalf("write corrupt db: %v", err)
	}
	cmd := newImportScanCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "opening local database") {
		t.Fatalf("import scan error = %v, want contextual store-open failure", err)
	}
	if ExitCode(err) == 9 {
		t.Fatalf("corrupt store reported as an unmet precondition: %v", err)
	}
}

// The refusal must not over-reach: with a synced mirror the scan classifies,
// and a manifest refresh runs with no mirror at all.
func TestIprMirrorDependentImportsRunWhenServable(t *testing.T) {
	t.Run("scan with synced mirror", func(t *testing.T) {
		root, _, out, _ := newPreflightTestRoot(t)
		iprSeedDemoStore(t)
		root.SetArgs([]string{"--json", "import", "scan", iprWriteDOIPDF(t)})
		if err := root.Execute(); err != nil {
			t.Fatalf("import scan with a synced mirror: %v; output=%q", err, out.String())
		}
		var report scanReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("decode scan report %q: %v", out.String(), err)
		}
		if len(report.Results) != 1 || report.Results[0].Status != "new" || report.Results[0].DOI != "10.1234/demo" {
			t.Fatalf("results = %+v, want one new 10.1234/demo", report.Results)
		}
	})
	t.Run("manifest refresh without mirror", func(t *testing.T) {
		root, _, out, _ := newPreflightTestRoot(t)
		srv := importResolveCrossRefWorkServer(t, "Refreshed Without Mirror", "10.1234/demo")
		withBase(t, &enrichCrossRefBase, srv.URL)

		var manifest bytes.Buffer
		if err := writeImportManifest(&manifest, importManifest{
			SchemaVersion: importManifestSchemaVersion,
			Entries: []importManifestEntry{{
				Path:           "/tmp/paper.pdf",
				Classification: "new",
				Action:         "create",
				IdentifierType: "doi",
				Identifier:     "10.1234/demo",
				Status:         "unresolved",
			}},
		}); err != nil {
			t.Fatalf("encode manifest: %v", err)
		}
		manifestPath := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.WriteFile(manifestPath, manifest.Bytes(), 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}

		root.SetArgs([]string{"--json", "--timeout", "5s", "import", "resolve", manifestPath})
		if err := root.Execute(); err != nil {
			t.Fatalf("manifest refresh without a mirror: %v; output=%q", err, out.String())
		}
		var got importManifest
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("decode manifest %q: %v", out.String(), err)
		}
		if len(got.Entries) != 1 || got.Entries[0].Status != "resolved" || got.Entries[0].Item["title"] != "Refreshed Without Mirror" {
			t.Fatalf("entries = %+v, want the refreshed create", got.Entries)
		}
	})
}
