// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

type exportResponse struct {
	data json.RawMessage
	err  error
}

// exportClientStub serves one recorded page per path: a request past the first
// page gets an empty page, the way the API answers a start beyond the end.
// Multi-page fixtures use exportPageStub instead.
type exportClientStub map[string]exportResponse

func (c exportClientStub) Get(path string, params map[string]string) (json.RawMessage, error) {
	response, ok := c[path]
	if !ok {
		return nil, errors.New("unexpected path: " + path)
	}
	if start := params["start"]; start != "" && start != "0" {
		return json.RawMessage(`[]`), nil
	}
	return response.data, response.err
}

func (c exportClientStub) GetWithHeader(path string, params map[string]string, _ string) (json.RawMessage, string, error) {
	data, err := c.Get(path, params)
	return data, "", err
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

// exportPageStub answers start/limit the way the API does, so a walk that
// stops after page one is visible as missing records rather than as a passing
// test against a stub that ignores pagination.
type exportPageStub struct {
	items       map[string][]string
	subcols     map[string][]string
	noTotal     bool
	ignoreStart bool
	starts      []string
	limits      []string
}

func (s *exportPageStub) resolve(path string, params map[string]string) (json.RawMessage, string, error) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/collections/"), "/", 2)
	if len(parts) != 2 {
		return nil, "", errors.New("unexpected path: " + path)
	}
	key, kind := parts[0], parts[1]
	s.starts = append(s.starts, params["start"])
	s.limits = append(s.limits, params["limit"])

	limit, _ := strconv.Atoi(params["limit"])
	start, _ := strconv.Atoi(params["start"])
	if s.ignoreStart {
		start = 0
	}
	window := func(n int) (int, int) {
		if start >= n {
			return 0, 0
		}
		end := start + limit
		if limit <= 0 || end > n {
			end = n
		}
		return start, end
	}

	switch kind {
	case "items":
		records := s.items[key]
		total := strconv.Itoa(len(records))
		if s.noTotal {
			total = ""
		}
		lo, hi := window(len(records))
		// format=keys stays countable for every item type, which is exactly why
		// the header-free walk asks for it.
		if params["format"] == "keys" {
			return json.RawMessage(strings.Join(records[lo:hi], "\n")), total, nil
		}
		if params["format"] == "csljson" {
			page := make([]json.RawMessage, 0, hi-lo)
			for _, id := range records[lo:hi] {
				page = append(page, json.RawMessage(`{"id":"`+id+`"}`))
			}
			body, err := json.Marshal(page)
			return body, total, err
		}
		var b strings.Builder
		for _, id := range records[lo:hi] {
			// An export format renders nothing for an item it cannot represent,
			// so a page of these is blank without the collection having ended.
			if strings.HasPrefix(id, "attachment") {
				continue
			}
			fmt.Fprintf(&b, "@article{%s}", id)
		}
		return json.RawMessage(b.String()), total, nil
	case "collections":
		children := s.subcols[key]
		lo, hi := window(len(children))
		page := make([]map[string]string, 0, hi-lo)
		for _, child := range children[lo:hi] {
			page = append(page, map[string]string{"key": child})
		}
		body, err := json.Marshal(page)
		return body, strconv.Itoa(len(children)), err
	}
	return nil, "", errors.New("unexpected path: " + path)
}

func (s *exportPageStub) Get(path string, params map[string]string) (json.RawMessage, error) {
	data, _, err := s.resolve(path, params)
	return data, err
}

func (s *exportPageStub) GetWithHeader(path string, params map[string]string, _ string) (json.RawMessage, string, error) {
	return s.resolve(path, params)
}

func manyIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%03d", prefix, i)
	}
	return ids
}

// A collection larger than one page used to export its first page and stop,
// with no error and no warning -- the export simply lost every item past the
// API's page cap.
func TestExportCollectionWalksEveryItemPage(t *testing.T) {
	ids := manyIDs("item", 250)
	client := &exportPageStub{
		items:   map[string][]string{"ROOT": ids},
		subcols: map[string][]string{"ROOT": nil},
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", true, 0, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	for _, id := range ids {
		if !strings.Contains(out.String(), "@article{"+id+"}") {
			t.Fatalf("export dropped %s", id)
		}
	}
	if got := client.starts; len(got) != 3 || got[0] != "0" || got[1] != "100" || got[2] != "200" {
		t.Fatalf("item page starts = %v, want 0/100/200", got)
	}
}

// --limit is a page size, not a cap on the export: the API clamps anything
// over 100, so asking for 200 must not become a 200-item ceiling.
func TestExportCollectionClampsPageSizeAndStillExportsEverything(t *testing.T) {
	ids := manyIDs("item", 250)
	client := &exportPageStub{
		items:   map[string][]string{"ROOT": ids},
		subcols: map[string][]string{"ROOT": nil},
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", true, 200, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if !strings.Contains(out.String(), "@article{item249}") {
		t.Fatal("export stopped before the last item")
	}
	for _, limit := range client.limits {
		if limit != "100" {
			t.Fatalf("requested limit = %s, want the API maximum of 100", limit)
		}
	}
}

// Total-Results drives the walk, but a server that omits it must still be
// paged to exhaustion rather than truncated at page one.
func TestExportCollectionWalksEveryPageWithoutTotalResults(t *testing.T) {
	ids := manyIDs("item", 250)
	client := &exportPageStub{
		items:   map[string][]string{"ROOT": ids},
		subcols: map[string][]string{"ROOT": nil},
		noTotal: true,
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", true, 0, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if !strings.Contains(out.String(), "@article{item249}") {
		t.Fatal("header-free walk stopped early")
	}
}

// A server that ignores start would otherwise stream page one forever, filling
// the output file until the disk does.
func TestExportCollectionStopsWhenTheServerIgnoresStart(t *testing.T) {
	client := &exportPageStub{
		items:       map[string][]string{"ROOT": manyIDs("item", 250)},
		subcols:     map[string][]string{"ROOT": nil},
		noTotal:     true,
		ignoreStart: true,
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", true, 0, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if got := strings.Count(out.String(), "@article{item000}"); got != 1 {
		t.Fatalf("emitted the repeated page %d times, want 1", got)
	}
}

func TestCollectCollectionCSLJSONWalksEveryItemPage(t *testing.T) {
	client := &exportPageStub{
		items:   map[string][]string{"ROOT": manyIDs("item", 250)},
		subcols: map[string][]string{"ROOT": nil},
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "csljson", true, 0, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	var items []map[string]string
	if err := json.Unmarshal(out.Bytes(), &items); err != nil {
		t.Fatalf("output is not a CSL-JSON array: %v", err)
	}
	if len(items) != 250 {
		t.Fatalf("exported %d items, want 250", len(items))
	}
}

// The subcollection read was unpaginated, so a broad collection lost whole
// subtrees: every child past the API's default page size was never visited.
func TestExportCollectionWalksEverySubcollectionPage(t *testing.T) {
	children := manyIDs("SUB", 150)
	items := map[string][]string{"ROOT": nil}
	subcols := map[string][]string{"ROOT": children}
	for _, child := range children {
		items[child] = []string{child + "-item"}
		subcols[child] = nil
	}
	client := &exportPageStub{items: items, subcols: subcols}

	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", false, 0, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	for _, child := range children {
		if !strings.Contains(out.String(), "@article{"+child+"-item}") {
			t.Fatalf("export never visited subcollection %s", child)
		}
	}
}

// Without Total-Results the walk cannot read termination out of the page body:
// BibTeX renders nothing for an attachment, so a whole page of them is blank
// in the middle of a full collection. Stopping there would silently drop
// everything after it, which is the same class of bug as the single-shot
// export this pagination replaced.
func TestExportCollectionKeepsGoingPastABlankMiddlePageWithoutTotalResults(t *testing.T) {
	records := append(manyIDs("attachment", 100), manyIDs("item", 50)...)
	client := &exportPageStub{
		items:   map[string][]string{"ROOT": records},
		subcols: map[string][]string{"ROOT": nil},
		noTotal: true,
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", true, 0, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	for _, id := range manyIDs("item", 50) {
		if !strings.Contains(out.String(), "@article{"+id+"}") {
			t.Fatalf("export stopped at the blank attachment page and dropped %s", id)
		}
	}
}
