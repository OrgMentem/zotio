// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): cover local-store research bundle export for collections.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotero-pp-cli/internal/store"
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
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotero-pp-cli"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

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
}

func readBundleTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}
