// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
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
	items           map[string][]string
	bibtex          map[string]string
	subcols         map[string][]string
	noTotal         bool
	ignoreStart     bool
	clampLateStarts bool
	formats         []string
	starts          []string
	limits          []string
}

func (s *exportPageStub) resolve(path string, params map[string]string) (json.RawMessage, string, error) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/collections/"), "/", 2)
	if len(parts) != 2 {
		return nil, "", errors.New("unexpected path: " + path)
	}
	key, kind := parts[0], parts[1]
	s.starts = append(s.starts, params["start"])
	s.limits = append(s.limits, params["limit"])
	s.formats = append(s.formats, params["format"])

	limit, _ := strconv.Atoi(params["limit"])
	start, _ := strconv.Atoi(params["start"])
	if s.ignoreStart {
		start = 0
	} else if s.clampLateStarts && start >= 100 {
		start = 100
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
			if citationKey, ok := s.bibtex[id]; ok {
				fmt.Fprintf(&b, "@article{%s,\n  title = {%s},\n}", citationKey, citationKey)
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
	if got := client.starts; len(got) != 6 || got[0] != "0" || got[1] != "0" || got[2] != "100" || got[3] != "100" || got[4] != "200" || got[5] != "200" {
		t.Fatalf("item page starts = %v, want 0/0/100/100/200/200", got)
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
	for i, limit := range client.limits {
		if client.formats[i] == "keys" {
			continue
		}
		if limit != "100" {
			t.Fatalf("requested export limit = %s, want the API maximum of 100", limit)
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

// A server that ignores start cannot be treated as a successful one-page
// export: the keys probe proves it returned the first item at a later offset.
func TestExportCollectionErrorsWhenTheServerIgnoresStart(t *testing.T) {
	client := &exportPageStub{
		items:       map[string][]string{"ROOT": manyIDs("item", 250)},
		subcols:     map[string][]string{"ROOT": nil},
		ignoreStart: true,
	}
	var out bytes.Buffer
	err := exportCollection(client, &out, "ROOT", "bibtex", true, 0, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "ignored start") {
		t.Fatalf("exportCollection error = %v, want ignored-start failure", err)
	}
}

// A server can honor the first page boundary and clamp every later request.
// Every probed page key must be distinct; comparing only with offset zero
// would accept the repeated second page.
func TestExportCollectionErrorsWhenServerClampsAtLaterOffset(t *testing.T) {
	for _, noTotal := range []bool{false, true} {
		t.Run(fmt.Sprintf("no_total=%t", noTotal), func(t *testing.T) {
			client := &exportPageStub{
				items:           map[string][]string{"ROOT": manyIDs("item", 350)},
				subcols:         map[string][]string{"ROOT": nil},
				noTotal:         noTotal,
				clampLateStarts: true,
			}
			var out bytes.Buffer
			err := exportCollection(client, &out, "ROOT", "bibtex", true, 0, map[string]bool{})
			if err == nil || !strings.Contains(err.Error(), "ignored start") {
				t.Fatalf("exportCollection error = %v, want ignored-start failure", err)
			}
		})
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

// The CSL-JSON walk used to `break` on a repeated page, so a server that
// ignores `start` produced a complete-looking JSON array holding only the
// first page. The text walk already failed loudly here; this one committed
// silent truncation, and an atomic publisher would faithfully publish it.
func TestCollectCollectionCSLJSONErrorsWhenTheServerIgnoresStart(t *testing.T) {
	client := &exportPageStub{
		items:       map[string][]string{"ROOT": manyIDs("item", 250)},
		subcols:     map[string][]string{"ROOT": nil},
		ignoreStart: true,
	}
	var out bytes.Buffer
	err := exportCollection(client, &out, "ROOT", "csljson", true, 0, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "ignored start") {
		t.Fatalf("exportCollection error = %v, want ignored-start failure", err)
	}
	// Nothing may reach the writer: a partial array is worse than no array,
	// because it parses.
	if out.Len() != 0 {
		t.Fatalf("truncated CSL-JSON was emitted despite the error: %q", out.String())
	}
}

// The subcollection walk used to `return keys, nil` on a repeated page, so a
// server that ignores `start` silently dropped every subtree past page one
// while reporting success.
func TestExportCollectionErrorsWhenSubcollectionPagingIgnoresStart(t *testing.T) {
	children := manyIDs("SUB", 150)
	items := map[string][]string{"ROOT": nil}
	subcols := map[string][]string{"ROOT": children}
	for _, child := range children {
		items[child] = []string{child + "-item"}
		subcols[child] = nil
	}
	client := &exportPageStub{items: items, subcols: subcols, ignoreStart: true}

	var out bytes.Buffer
	// flat=false, or exportCollection returns before it ever reads subcollections.
	err := exportCollection(client, &out, "ROOT", "bibtex", false, 0, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "ignored start") {
		t.Fatalf("exportCollection error = %v, want ignored-start failure", err)
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

// Two adjacent pages can be byte-identical when they contain only
// unrenderable attachments. Their opaque output is not a pagination signal.
func TestExportCollectionWalksPastConsecutiveBlankPages(t *testing.T) {
	records := append(manyIDs("attachment", 200), manyIDs("item", 50)...)
	client := &exportPageStub{
		items:   map[string][]string{"ROOT": records},
		subcols: map[string][]string{"ROOT": nil},
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", true, 0, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if !strings.Contains(out.String(), "@article{item049}") {
		t.Fatal("export stopped after consecutive blank pages")
	}
}

// Zotero resolves a duplicate citation key only within one translator call.
// Paginated output must preserve that uniqueness across calls.
func TestExportCollectionDeduplicatesBibTeXKeysAcrossPages(t *testing.T) {
	client := &exportPageStub{
		items:   map[string][]string{"ROOT": {"first", "second"}},
		subcols: map[string][]string{"ROOT": nil},
		bibtex:  map[string]string{"first": "same", "second": "same"},
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", true, 1, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if got := out.String(); strings.Count(got, "@article{same,") != 1 || strings.Count(got, "@article{same-1,") != 1 || strings.Count(got, "title = {same}") != 2 {
		t.Fatalf("BibTeX keys = %q, want distinct keys and unchanged field values", got)
	}
}

func TestDeduplicateBibTeXCitationKeysPreservesAtSignsInFieldValues(t *testing.T) {
	input := `@article{same,
  annote = {Contact a@example.com \textit{urgent}, follow up},
  title = {First}
}
@article{same,
  annote = {Contact a@example.com \textit{urgent}, follow up},
  title = {Second}
}`
	want := `@article{same,
  annote = {Contact a@example.com \textit{urgent}, follow up},
  title = {First}
}
@article{same-1,
  annote = {Contact a@example.com \textit{urgent}, follow up},
  title = {Second}
}`
	if got := deduplicateBibTeXCitationKeys(input, make(map[string]bool)); got != want {
		t.Fatalf("deduplicateBibTeXCitationKeys corrupted field content:\n%s\nwant:\n%s", got, want)
	}
}

func TestDeduplicateBibTeXCitationKeysIgnoresEntrySyntaxInsideFieldValues(t *testing.T) {
	input := `@article{same,
  annote = {The literal @article{not-a-record, value} belongs in this note}
}
@article{same,
  annote = {The literal @article{not-a-record, value} belongs in this note}
}`
	want := `@article{same,
  annote = {The literal @article{not-a-record, value} belongs in this note}
}
@article{same-1,
  annote = {The literal @article{not-a-record, value} belongs in this note}
}`
	if got := deduplicateBibTeXCitationKeys(input, make(map[string]bool)); got != want {
		t.Fatalf("deduplicateBibTeXCitationKeys rewrote field content:\n%s\nwant:\n%s", got, want)
	}
}

func TestDeduplicateBibTeXCitationKeysLeavesNonRecordsAndCommentsUntouched(t *testing.T) {
	input := `@comment{same, metadata}
@comment{same, metadata}
@preamble{"one, two"}
@preamble{"one, two"}
@string{abbr = "one, two"}
@string{abbr = "one, two"}
% @article{fake, title = {commented}}
@article
{fake, title = {real}}
@article
{fake, title = {real duplicate}}`
	want := `@comment{same, metadata}
@comment{same, metadata}
@preamble{"one, two"}
@preamble{"one, two"}
@string{abbr = "one, two"}
@string{abbr = "one, two"}
% @article{fake, title = {commented}}
@article
{fake, title = {real}}
@article
{fake-1, title = {real duplicate}}`
	if got := deduplicateBibTeXCitationKeys(input, make(map[string]bool)); got != want {
		t.Fatalf("deduplicateBibTeXCitationKeys rewrote a non-record:\n%s\nwant:\n%s", got, want)
	}
}

func TestDeduplicateBibTeXCitationKeysHonorsEscapedBraces(t *testing.T) {
	input := `@article{same,
  annote = {Contact \} a@example.com \textit{urgent}, follow up}
}
@article{same,
  annote = {Contact \} a@example.com \textit{urgent}, follow up}
}`
	want := `@article{same,
  annote = {Contact \} a@example.com \textit{urgent}, follow up}
}
@article{same-1,
  annote = {Contact \} a@example.com \textit{urgent}, follow up}
}`
	if got := deduplicateBibTeXCitationKeys(input, make(map[string]bool)); got != want {
		t.Fatalf("deduplicateBibTeXCitationKeys misread escaped braces:\n%s\nwant:\n%s", got, want)
	}
}

func TestExportCollectionDeduplicatesBibTeXKeysAcrossSubcollections(t *testing.T) {
	client := &exportPageStub{
		items: map[string][]string{
			"ROOT": {"root"},
			"SUB":  {"sub"},
		},
		subcols: map[string][]string{
			"ROOT": {"SUB"},
			"SUB":  nil,
		},
		bibtex: map[string]string{"root": "same", "sub": "same"},
	}
	var out bytes.Buffer
	if err := exportCollection(client, &out, "ROOT", "bibtex", false, 1, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if got := out.String(); strings.Count(got, "@article{same,") != 1 || strings.Count(got, "@article{same-1,") != 1 {
		t.Fatalf("BibTeX keys = %q, want same and same-1", got)
	}
}

// collectionExportFixtureItems are stored exactly as the API serves them, so a
// mirror-backed export and a live export can be compared on identical rows.
var collectionExportFixtureItems = []json.RawMessage{
	json.RawMessage(`{"key":"IROOT","version":3,"data":{"key":"IROOT","itemType":"journalArticle","title":"Root Item","collections":["ROOT"]}}`),
	json.RawMessage(`{"key":"ISUB","version":4,"data":{"key":"ISUB","itemType":"book","title":"Sub Item","collections":["SUB"]}}`),
	json.RawMessage(`{"key":"IELSE","version":5,"data":{"key":"IELSE","itemType":"book","title":"Unrelated","collections":["OTHER"]}}`),
}

var collectionExportFixtureCollections = []json.RawMessage{
	json.RawMessage(`{"key":"ROOT","version":1,"data":{"key":"ROOT","name":"Root","parentCollection":false}}`),
	json.RawMessage(`{"key":"SUB","version":2,"data":{"key":"SUB","name":"Sub","parentCollection":"ROOT"}}`),
	json.RawMessage(`{"key":"OTHER","version":6,"data":{"key":"OTHER","name":"Other","parentCollection":false}}`),
}

// seedCollectionExportStore mirrors a two-level collection tree plus one
// unrelated collection, so a local export that ignored the requested scope
// would show up as a leaked item rather than as a passing test.
func seedCollectionExportStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", collectionExportFixtureItems); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if _, _, err := db.UpsertBatch("collections", collectionExportFixtureCollections); err != nil {
		t.Fatalf("seed collections: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// newCollectionExportServer serves the same subtree the mirror holds, using the
// package's httptest Zotero pattern.
func newCollectionExportServer(t *testing.T) *httptest.Server {
	t.Helper()
	byCollection := map[string][]json.RawMessage{
		"ROOT": {collectionExportFixtureItems[0]},
		"SUB":  {collectionExportFixtureItems[1]},
	}
	children := map[string][]json.RawMessage{
		"ROOT": {collectionExportFixtureCollections[1]},
		"SUB":  {},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encode := func(rows []json.RawMessage) {
			if rows == nil {
				rows = []json.RawMessage{}
			}
			w.Header().Set("Total-Results", strconv.Itoa(len(rows)))
			if start := r.URL.Query().Get("start"); start != "" && start != "0" {
				rows = []json.RawMessage{}
			}
			body, err := json.Marshal(rows)
			if err != nil {
				t.Errorf("encode fixture rows: %v", err)
				return
			}
			_, _ = w.Write(body)
		}
		switch path := strings.TrimPrefix(r.URL.Path, "/users/0"); path {
		case "/collections/ROOT/items", "/collections/SUB/items":
			encode(byCollection[strings.Split(path, "/")[2]])
		case "/collections/ROOT/collections", "/collections/SUB/collections":
			encode(children[strings.Split(path, "/")[2]])
		default:
			t.Errorf("unexpected live request %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runCollectionsExportCmd(t *testing.T, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newCollectionsExportCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func exportedItemKeys(t *testing.T, out string) []string {
	t.Helper()
	var rows []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("export output is not a JSON array (%v): %q", err, out)
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Key)
	}
	sort.Strings(keys)
	return keys
}

// TestCollectionsExportJSONLocalMatchesLivePath is the offline promise: with
// Zotero closed and a synced store, the subtree export still resolves the same
// items. It compares the mirror-backed run against the live run rather than
// against a hand-written expectation, so a scope bug on either side fails here.
func TestCollectionsExportJSONLocalMatchesLivePath(t *testing.T) {
	seedCollectionExportStore(t)
	srv := newCollectionExportServer(t)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	liveOut, err := runCollectionsExportCmd(t, &rootFlags{dataSource: "live", noCache: true, timeout: 5 * time.Second}, "ROOT", "--format", "json")
	if err != nil {
		t.Fatalf("live export: %v", err)
	}
	liveKeys := exportedItemKeys(t, liveOut)
	if len(liveKeys) != 2 {
		t.Fatalf("live export keys = %v, want the root item and the subcollection item", liveKeys)
	}

	// Zotero closed: the base URL now points at a port nothing serves, so any
	// live read would fail rather than quietly answer.
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")
	localOut, err := runCollectionsExportCmd(t, &rootFlags{dataSource: "local", noCache: true, timeout: 5 * time.Second}, "ROOT", "--format", "json")
	if err != nil {
		t.Fatalf("local export with Zotero closed: %v", err)
	}
	localKeys := exportedItemKeys(t, localOut)
	if !reflect.DeepEqual(localKeys, liveKeys) {
		t.Fatalf("local export keys = %v, live = %v", localKeys, liveKeys)
	}
	if strings.Contains(localOut, "IELSE") {
		t.Fatalf("local export leaked an item from an unrelated collection: %q", localOut)
	}
}

// TestCollectionsExportJSONLocalHonorsFlat proves the local subcollection
// planner is what descends the tree: --flat must stop at the root.
func TestCollectionsExportJSONLocalHonorsFlat(t *testing.T) {
	seedCollectionExportStore(t)
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")

	out, err := runCollectionsExportCmd(t, &rootFlags{dataSource: "local", noCache: true, timeout: 5 * time.Second}, "ROOT", "--format", "json", "--flat")
	if err != nil {
		t.Fatalf("flat local export: %v", err)
	}
	if got := exportedItemKeys(t, out); !reflect.DeepEqual(got, []string{"IROOT"}) {
		t.Fatalf("flat local export keys = %v, want [IROOT]", got)
	}
}

// TestCollectionsExportTranslatorFormatsRefuseOffline covers the deliberate
// non-goal: bibtex/ris/csljson come from Zotero's export translators, which the
// mirror cannot render. A local .bib would carry citation keys that Better
// BibTeX never assigned, so the refusal is the product decision — and it has to
// name the offline route rather than just failing.
func TestCollectionsExportTranslatorFormatsRefuseOffline(t *testing.T) {
	for _, format := range []string{"bibtex", "ris", "csljson"} {
		t.Run(format, func(t *testing.T) {
			seedCollectionExportStore(t)
			t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")

			out, err := runCollectionsExportCmd(t, &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: 5 * time.Second}, "ROOT", "--format", format)
			if err == nil {
				t.Fatalf("--format %s --data-source local succeeded; output %q", format, out)
			}
			assertPreconditionExitCode(t, err, 9)
			var env preconditionUnmetEnvelope
			if decodeErr := json.Unmarshal([]byte(out), &env); decodeErr != nil {
				t.Fatalf("output is not a precondition_unmet envelope; decode %q: %v", out, decodeErr)
			}
			if env.Kind != "precondition_unmet" || env.Precondition != preconditionLiveLocalAPI {
				t.Fatalf("envelope = %+v, want precondition_unmet/live_local_api", env)
			}
			if env.Capability != "collections export" {
				t.Fatalf("capability = %q, want collections export", env.Capability)
			}
			if !strings.Contains(strings.Join(env.Remediation, " "), "--format json") {
				t.Fatalf("remediation does not name the offline route: %v", env.Remediation)
			}
		})
	}
}

// TestCollectionsExportTranslatorFormatRefusesWhenZoteroGoesAway covers auto
// mode: a translator export must not degrade into a mirror dump when the
// desktop stops answering mid-flight.
func TestCollectionsExportTranslatorFormatRefusesWhenZoteroGoesAway(t *testing.T) {
	seedCollectionExportStore(t)
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")

	out, err := runCollectionsExportCmd(t, &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second}, "ROOT", "--format", "bibtex")
	if err == nil {
		t.Fatalf("auto-mode bibtex export succeeded with Zotero unreachable; output %q", out)
	}
	assertPreconditionExitCode(t, err, 9)
	if !strings.Contains(out, "precondition_unmet") {
		t.Fatalf("output = %q, want a precondition_unmet envelope", out)
	}
	if strings.Contains(out, "Root Item") {
		t.Fatalf("translator export degraded into mirrored item JSON: %q", out)
	}
}
