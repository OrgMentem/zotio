// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

func TestCollectionsBundleWritesResearchPackage(t *testing.T) {
	seedCollectionBundleStore(t)

	outDir := filepath.Join(t.TempDir(), "bundle")
	flags := &rootFlags{}
	cmd := newCollectionsCmd(flags)
	cmd.SetArgs([]string{"bundle", "COL", "--out", outDir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if !strings.Contains(out.String(), "2 item(s)") {
		t.Fatalf("summary = %q, want item count", out.String())
	}

	synthesis := readBundleTestFile(t, outDir, "synthesis.md")
	annotations := readBundleTestFile(t, outDir, "annotations.md")
	bibliography := readBundleTestFile(t, outDir, "bibliography.json")

	for _, want := range []string{"Attention Is All You Need", "Graph Retrieval for Notes", "K1", "K2", collectionSynthesisPrompt(2)} {
		if !strings.Contains(synthesis, want) {
			t.Errorf("synthesis.md missing %q:\n%s", want, synthesis)
		}
	}
	for _, want := range []string{"Attention Is All You Need", "K1", "self-attention is the key observation"} {
		if !strings.Contains(annotations, want) {
			t.Errorf("annotations.md missing %q:\n%s", want, annotations)
		}
	}
	for _, want := range []string{"K1", "K2", "Attention Is All You Need", "Graph Retrieval for Notes"} {
		if !strings.Contains(bibliography, want) {
			t.Errorf("bibliography.json missing %q:\n%s", want, bibliography)
		}
	}

	for _, name := range []string{"synthesis.md", "annotations.md", "bibliography.json"} {
		assertFileMode(t, filepath.Join(outDir, name), 0o600)
	}
}

func TestCollectionsBundleSameOutputReturnsBusyAndPreservesFirstArtifacts(t *testing.T) {
	seedCollectionBundleStore(t)
	outDir := filepath.Join(t.TempDir(), "bundle")
	canonicalOut, err := canonicalOutputPath(outDir)
	if err != nil {
		t.Fatalf("canonical output: %v", err)
	}

	firstFlags := &rootFlags{}
	first := newCollectionsBundleCmd(firstFlags)
	first.SetContext(context.Background())
	first.SetOut(&bytes.Buffer{})
	first.SetErr(&bytes.Buffer{})
	err = withPathWriterLock(first, canonicalOut+".lock", "collections bundle", func() error {
		second := newCollectionsCmd(&rootFlags{})
		second.SetArgs([]string{"bundle", "OTHER", "--out", outDir})
		second.SetOut(&bytes.Buffer{})
		second.SetErr(&bytes.Buffer{})
		busyErr := second.Execute()
		switch {
		case busyErr == nil:
			return errors.New("second bundle succeeded; want busy precondition exit 9")
		case ExitCode(busyErr) != 9:
			return fmt.Errorf("second bundle exit = %d, want busy precondition exit 9: %w", ExitCode(busyErr), busyErr)
		}
		return runCollectionsBundle(first, firstFlags, "COL", outDir)
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"synthesis.md", "annotations.md", "bibliography.json"} {
		if data := readBundleTestFile(t, outDir, name); !strings.Contains(data, "Attention Is All You Need") {
			t.Fatalf("%s does not contain the first fixture: %q", name, data)
		}
	}
}

func TestCollectionsBundleIncludesStoredFulltext(t *testing.T) {
	seedCollectionBundleStore(t)

	outDir := filepath.Join(t.TempDir(), "bundle")
	cmd := newCollectionsCmd(&rootFlags{})
	cmd.SetArgs([]string{"bundle", "COL", "--out", outDir})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bundle: %v", err)
	}

	if synthesis := readBundleTestFile(t, outDir, "synthesis.md"); !strings.Contains(synthesis, "transformer full text") {
		t.Fatalf("synthesis.md missing stored fulltext:\n%s", synthesis)
	}
}

func TestCollectionsBundlePropagatesFulltextReadError(t *testing.T) {
	db := seedCollectionBundleStoreOpen(t)
	defer db.Close()

	if _, err := db.DB().Exec(`ALTER TABLE resources RENAME TO resources_original`); err != nil {
		t.Fatalf("rename resources table: %v", err)
	}
	if _, err := db.DB().Exec(`
		CREATE TABLE resources (
			id TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			data JSON,
			parent_key TEXT,
			item_type TEXT,
			annotation_color TEXT,
			item_date TEXT,
			synced_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (resource_type, id)
		)
	`); err != nil {
		t.Fatalf("create nullable resources table: %v", err)
	}
	if _, err := db.DB().Exec(`
		INSERT INTO resources (id, resource_type, data, parent_key, item_type, annotation_color, item_date, synced_at, updated_at)
		SELECT id, resource_type, data, parent_key, item_type, annotation_color, item_date, synced_at, updated_at
		FROM resources_original
	`); err != nil {
		t.Fatalf("copy resources: %v", err)
	}
	if _, err := db.DB().Exec(`DROP TABLE resources_original`); err != nil {
		t.Fatalf("drop original resources table: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE resources SET data = NULL WHERE resource_type = 'fulltext' AND id = 'ATT1'`); err != nil {
		t.Fatalf("corrupt stored fulltext: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "bundle")
	if _, err := writeCollectionBundle(db, "COL", outDir); err == nil {
		t.Fatal("writeCollectionBundle succeeded after fulltext read failure")
	} else if !strings.Contains(err.Error(), "reading collection fulltext") {
		t.Fatalf("writeCollectionBundle error = %v, want fulltext read error", err)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Errorf("bundle output directory exists after fulltext read failure: %v", err)
	}
}

func TestCollectionsBundleJSONManifest(t *testing.T) {
	seedCollectionBundleStore(t)

	outDir := filepath.Join(t.TempDir(), "bundle")
	flags := &rootFlags{asJSON: true}
	cmd := newCollectionsCmd(flags)
	cmd.SetArgs([]string{"bundle", "COL", "--out", outDir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bundle json: %v", err)
	}

	var manifest collectionBundleManifest
	if err := json.Unmarshal(out.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest %q: %v", out.String(), err)
	}
	if manifest.Collection != "COL" || manifest.ItemCount != 2 || manifest.Out != outDir {
		t.Fatalf("manifest = %+v", manifest)
	}
	if strings.Join(manifest.Files, ",") != "synthesis.md,annotations.md,bibliography.json" {
		t.Fatalf("files = %v", manifest.Files)
	}
}

func seedCollectionBundleStore(t *testing.T) {
	t.Helper()
	db := seedCollectionBundleStoreOpen(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func seedCollectionBundleStoreOpen(t *testing.T) *store.Store {
	t.Helper()
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	collections := []json.RawMessage{
		json.RawMessage(`{"key":"COL","version":1,"data":{"key":"COL","name":"Reading List"}}`),
	}
	if _, _, err := db.UpsertBatch("collections", collections); err != nil {
		t.Fatalf("seed collections: %v", err)
	}

	items := []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":2,"data":{"key":"K1","itemType":"journalArticle","title":"Attention Is All You Need","creators":[{"lastName":"Vaswani","firstName":"Ashish","creatorType":"author"}],"date":"2017","publicationTitle":"NeurIPS","abstractNote":"We propose the Transformer architecture.","collections":["COL"]}}`),
		json.RawMessage(`{"key":"K2","version":3,"data":{"key":"K2","itemType":"journalArticle","title":"Graph Retrieval for Notes","creators":[{"lastName":"Rivera","firstName":"Maya","creatorType":"author"}],"date":"2024","publicationTitle":"Notebook Systems","abstractNote":"Graph retrieval improves research recall.","collections":["COL"]}}`),
		json.RawMessage(`{"key":"ATT1","version":4,"data":{"key":"ATT1","itemType":"attachment","title":"K1 PDF","parentItem":"K1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"ANN1","version":5,"data":{"key":"ANN1","itemType":"annotation","parentItem":"ATT1","annotationType":"highlight","annotationText":"self-attention is the key observation","annotationComment":"central claim","annotationPageLabel":"3","dateAdded":"2026-01-02T03:04:05Z"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.UpsertKeyed("fulltext", []string{"ATT1"}, []json.RawMessage{
		json.RawMessage(`{"content":"transformer full text"}`),
	}); err != nil {
		t.Fatalf("seed fulltext: %v", err)
	}
	return db
}

func readBundleTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
