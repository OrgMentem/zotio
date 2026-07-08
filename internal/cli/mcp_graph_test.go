// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

type graphCollectionTestNode struct {
	Key            string                    `json:"key"`
	Name           string                    `json:"name"`
	Subcollections []graphCollectionTestNode `json:"subcollections"`
	Truncated      bool                      `json:"truncated"`
}

type graphChildTestNode struct {
	Key         string `json:"key"`
	ItemType    string `json:"item_type"`
	Title       string `json:"title"`
	ContentType string `json:"content_type"`
	LinkMode    string `json:"link_mode"`
}

type graphChildrenTestPayload struct {
	Key       string               `json:"key"`
	Children  []graphChildTestNode `json:"children"`
	Truncated bool                 `json:"truncated"`
}

type graphAttachmentsTestPayload struct {
	Key         string               `json:"key"`
	Attachments []graphChildTestNode `json:"attachments"`
	Truncated   bool                 `json:"truncated"`
}

type graphContextTestPayload struct {
	Key             string   `json:"key"`
	ItemType        string   `json:"item_type"`
	Title           string   `json:"title"`
	Parent          string   `json:"parent"`
	Collections     []string `json:"collections"`
	Tags            []string `json:"tags"`
	ChildCount      int      `json:"child_count"`
	AttachmentCount int      `json:"attachment_count"`
	Truncated       bool     `json:"truncated"`
}

func TestMCPGraphExports(t *testing.T) {
	seedMCPGraphStore(t)

	treeJSON, err := CollectionTreeJSON("COLL1")
	if err != nil {
		t.Fatalf("CollectionTreeJSON: %v", err)
	}
	var tree graphCollectionTestNode
	if err := json.Unmarshal(treeJSON, &tree); err != nil {
		t.Fatalf("decode collection tree: %v", err)
	}
	if tree.Key != "COLL1" || tree.Name != "Root" {
		t.Fatalf("collection root = %#v, want COLL1 Root", tree)
	}
	if len(tree.Subcollections) != 1 || tree.Subcollections[0].Key != "COLL2" || tree.Subcollections[0].Name != "Child" {
		t.Fatalf("collection children = %#v, want nested COLL2", tree.Subcollections)
	}

	childrenJSON, err := ItemChildrenJSON("P1")
	if err != nil {
		t.Fatalf("ItemChildrenJSON: %v", err)
	}
	var children graphChildrenTestPayload
	if err := json.Unmarshal(childrenJSON, &children); err != nil {
		t.Fatalf("decode item children: %v", err)
	}
	if len(children.Children) != 1 || children.Children[0].Key != "A1" || children.Children[0].ItemType != "attachment" {
		t.Fatalf("item children = %#v, want A1 attachment", children.Children)
	}

	attachmentsJSON, err := ItemAttachmentsJSON("P1")
	if err != nil {
		t.Fatalf("ItemAttachmentsJSON: %v", err)
	}
	var attachments graphAttachmentsTestPayload
	if err := json.Unmarshal(attachmentsJSON, &attachments); err != nil {
		t.Fatalf("decode item attachments: %v", err)
	}
	if len(attachments.Attachments) != 1 || attachments.Attachments[0].Key != "A1" || attachments.Attachments[0].ContentType != "application/pdf" {
		t.Fatalf("item attachments = %#v, want A1 application/pdf", attachments.Attachments)
	}

	contextJSON, err := ItemContextJSON("P1")
	if err != nil {
		t.Fatalf("ItemContextJSON: %v", err)
	}
	var itemContext graphContextTestPayload
	if err := json.Unmarshal(contextJSON, &itemContext); err != nil {
		t.Fatalf("decode item context: %v", err)
	}
	if itemContext.Key != "P1" || itemContext.Title != "Parent Item" {
		t.Fatalf("item context = %#v, want P1 Parent Item", itemContext)
	}
	if itemContext.AttachmentCount < 1 {
		t.Fatalf("attachment_count = %d, want >= 1", itemContext.AttachmentCount)
	}
}

func seedMCPGraphStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	collections := []json.RawMessage{
		json.RawMessage(`{"key":"COLL1","version":1,"data":{"key":"COLL1","name":"Root"}}`),
		json.RawMessage(`{"key":"COLL2","version":2,"data":{"key":"COLL2","name":"Child","parentCollection":"COLL1"}}`),
	}
	if _, _, err := db.UpsertBatch("collections", collections); err != nil {
		t.Fatalf("seed collections: %v", err)
	}

	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":3,"data":{"key":"P1","itemType":"journalArticle","title":"Parent Item","collections":["COLL1"],"tags":[{"tag":"graph"}]}}`),
		json.RawMessage(`{"key":"A1","version":4,"data":{"key":"A1","itemType":"attachment","title":"Attachment PDF","parentItem":"P1","contentType":"application/pdf","linkMode":"imported_file"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
}
