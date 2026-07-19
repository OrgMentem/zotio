// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type exportResponse struct {
	data json.RawMessage
	err  error
}

type exportClientStub map[string]exportResponse

func (c exportClientStub) Get(path string, _ map[string]string) (json.RawMessage, error) {
	response, ok := c[path]
	if !ok {
		return nil, errors.New("unexpected path: " + path)
	}
	return response.data, response.err
}

func TestExportCollectionRejectsMalformedSubcollections(t *testing.T) {
	client := exportClientStub{
		"/collections/ROOT/items":       {data: json.RawMessage(`[]`)},
		"/collections/ROOT/collections": {data: json.RawMessage(`{`)},
	}
	var out bytes.Buffer
	err := exportCollection(client, &out, "ROOT", "bibtex", false, 200, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "decoding subcollections for ROOT") {
		t.Fatalf("exportCollection error = %v, want contextual decode failure", err)
	}
}

func TestExportCollectionPropagatesRecursiveFailure(t *testing.T) {
	client := exportClientStub{
		"/collections/ROOT/items":       {data: json.RawMessage(`[]`)},
		"/collections/ROOT/collections": {data: json.RawMessage(`[{"key":"SUB"}]`)},
		"/collections/SUB/items":        {err: errors.New("network unavailable")},
	}
	var out bytes.Buffer
	err := exportCollection(client, &out, "ROOT", "bibtex", false, 200, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "exporting subcollection SUB") || !strings.Contains(err.Error(), "fetching items for collection SUB") {
		t.Fatalf("exportCollection error = %v, want recursive fetch context", err)
	}
}

func TestExportCollectionHealthyOutputUnchanged(t *testing.T) {
	client := exportClientStub{
		"/collections/ROOT/items":       {data: json.RawMessage(`@article{one}`)},
		"/collections/ROOT/collections": {data: json.RawMessage(`[]`)},
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", false, 200, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if got := out.String(); got != "@article{one}\n" {
		t.Fatalf("output = %q, want unchanged export", got)
	}
}

func TestExportCollectionCSLJSONCombinesRecursiveItems(t *testing.T) {
	client := exportClientStub{
		"/collections/ROOT/items":       {data: json.RawMessage(`[{"id":"root"}]`)},
		"/collections/ROOT/collections": {data: json.RawMessage(`[{"key":"SUB"}]`)},
		"/collections/SUB/items":        {data: json.RawMessage(`[{"id":"sub"}]`)},
		"/collections/SUB/collections":  {data: json.RawMessage(`[]`)},
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "csljson", false, 200, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	var items []map[string]string
	if err := json.Unmarshal(out.Bytes(), &items); err != nil {
		t.Fatalf("output is not a single CSL-JSON array: %q (%v)", out.String(), err)
	}
	if len(items) != 2 || items[0]["id"] != "root" || items[1]["id"] != "sub" {
		t.Fatalf("items = %#v, want root and sub", items)
	}
}
