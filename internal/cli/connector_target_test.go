// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/connector"
)

func TestConnectorTargetPaths(t *testing.T) {
	selected := connector.SelectedCollection{Targets: []connector.SelectedTarget{
		{ID: "L1", Name: "My Library", Level: 0, FilesEditable: true},
		{ID: "C1", Name: "Parent", Level: 1, FilesEditable: true},
		{ID: "C2", Name: "Child", Level: 2, FilesEditable: true},
		{ID: "C3", Name: "Sibling", Level: 1, FilesEditable: true},
	}}
	got := connectorTargetPaths(selected)
	paths := map[string]string{}
	for _, target := range got {
		paths[target.ID] = target.Path
	}
	want := map[string]string{"C1": "Parent", "C2": "Parent/Child", "C3": "Sibling"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestAPICollectionPaths(t *testing.T) {
	rows := []apiCollectionRow{
		collectionRow("PARENT", "Parent", nil),
		collectionRow("CHILD", "Child", "PARENT"),
		collectionRow("SIBLING", "Sibling", nil),
	}
	got := apiCollectionPaths(rows)
	want := map[string]string{"PARENT": "Parent", "CHILD": "Parent/Child", "SIBLING": "Sibling"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %#v, want %#v", got, want)
	}
}

func TestAPICollectionPathsCyclicParents(t *testing.T) {
	rows := []apiCollectionRow{
		collectionRow("SELF", "Self", "SELF"),
		collectionRow("A", "A", "B"),
		collectionRow("B", "B", "A"),
	}

	got := apiCollectionPaths(rows)
	if got["SELF"] != "Self" {
		t.Errorf("self-referencing path = %q, want %q", got["SELF"], "Self")
	}
	if got["A"] == "" || got["B"] == "" {
		t.Errorf("cyclic paths = %#v, want paths for both collections", got)
	}
}

func TestAPICollectionPathsSharedAncestor(t *testing.T) {
	rows := []apiCollectionRow{
		collectionRow("A", "A", nil),
		collectionRow("B", "B", "A"),
		collectionRow("C", "C", "A"),
		collectionRow("D", "D", "B"),
		collectionRow("E", "E", "C"),
	}

	got := apiCollectionPaths(rows)
	want := map[string]string{
		"A": "A",
		"B": "A/B",
		"C": "A/C",
		"D": "A/B/D",
		"E": "A/C/E",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diamond paths = %#v, want %#v", got, want)
	}
}

func collectionRow(key, name string, parent any) apiCollectionRow {
	var row apiCollectionRow
	row.Key = key
	row.Data.Key = key
	row.Data.Name = name
	row.Data.ParentCollection = parent
	return row
}

// fakeConnectorTarget is one row of the desktop connector's target tree, kept
// separate from connector.SelectedTarget so the fixture writes the real wire
// JSON instead of round-tripping the very struct the production decoder uses.
type fakeConnectorTarget struct {
	id    string
	name  string
	level int
}

// connectorTargetFake serves both halves of the write-routing decision: the
// Zotero Web API /collections list that apiCollectionPath paginates, and the
// desktop /connector/getSelectedCollection tree that resolveConnectorTarget
// matches against.
type connectorTargetFake struct {
	srv *httptest.Server
	// collectionPages[i] holds the raw row JSON returned for start=i*100.
	collectionPages    [][]string
	collectionsStatus  int
	collectionsBody    string
	targets            []fakeConnectorTarget
	collectionRequests atomic.Int64
	connectorRequests  atomic.Int64
}

func newConnectorTargetFake(t *testing.T) *connectorTargetFake {
	t.Helper()
	fake := &connectorTargetFake{}
	fake.srv = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.srv.Close)
	return fake
}

func (f *connectorTargetFake) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/users/0/collections":
		// The production collection listing is a Web API GET (client.Client.Get
		// routes /collections through do(ctx, "GET", ...)), so any other verb is
		// a request Zotero would reject and must not be answered with rows.
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"expected GET, got `+r.Method+`"}`, http.StatusMethodNotAllowed)
			return
		}
		// A paginator that never advances start would otherwise spin forever;
		// fail the request instead of hanging the test.
		if f.collectionRequests.Add(1) > 8 {
			http.Error(w, `{"error":"collection paging did not terminate"}`, http.StatusInternalServerError)
			return
		}
		if f.collectionsStatus != 0 && f.collectionsStatus != http.StatusOK {
			http.Error(w, `{"error":"denied"}`, f.collectionsStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if f.collectionsBody != "" {
			_, _ = w.Write([]byte(f.collectionsBody))
			return
		}
		start, err := strconv.Atoi(r.URL.Query().Get("start"))
		if err != nil {
			http.Error(w, `{"error":"missing start parameter"}`, http.StatusBadRequest)
			return
		}
		var page []string
		if index := start / 100; index >= 0 && index < len(f.collectionPages) {
			page = f.collectionPages[index]
		}
		_, _ = w.Write([]byte("[" + strings.Join(page, ",") + "]"))
	case "/connector/getSelectedCollection":
		// connector.Client.SelectedCollection posts "{}" to this endpoint
		// (internal/connector/connector.go), and the desktop only accepts POST.
		// Answering a GET here would hide a verb regression from every routing
		// case below.
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"expected POST, got `+r.Method+`"}`, http.StatusMethodNotAllowed)
			return
		}
		f.connectorRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(selectedCollectionJSON(f.targets)))
	default:
		http.Error(w, "unexpected request "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *connectorTargetFake) connectorClient() *connector.Client {
	return connector.New(f.srv.URL+"/connector", time.Second)
}

func (f *connectorTargetFake) flags(t *testing.T, connectorTarget string) *rootFlags {
	t.Helper()
	// config.Load applies ZOTERO_BASE_URL after it reads the config file, so an
	// inherited value would silently outrank configPath and point this test at
	// the operator's real Zotero desktop. ZOTIO_DEMO short-circuits Load before
	// the file is read at all, and ZOTERO_API_KEY would swap in a live
	// credential. Clear all three, following ensure_live_test.go.
	t.Setenv("ZOTIO_DEMO", "")
	t.Setenv("ZOTERO_BASE_URL", "")
	t.Setenv("ZOTERO_API_KEY", "")
	return &rootFlags{
		configPath:      testConfigFile(t, f.srv.URL+"/users/0"),
		connectorTarget: connectorTarget,
		timeout:         time.Second,
	}
}

// selectedCollectionJSON renders the desktop connector's getSelectedCollection
// payload exactly as Zotero sends it.
func selectedCollectionJSON(targets []fakeConnectorTarget) string {
	rendered := make([]string, 0, len(targets))
	for _, target := range targets {
		rendered = append(rendered, fmt.Sprintf(`{"id":%q,"name":%q,"level":%d,"filesEditable":true}`, target.id, target.name, target.level))
	}
	return `{"libraryID":1,"libraryName":"My Library","editable":true,"filesEditable":true,"id":null,"name":"My Library","targets":[` +
		strings.Join(rendered, ",") + `],"translatorMode":false}`
}

// apiCollectionRowJSON renders one Web API collection row. Zotero reports a
// top-level collection as parentCollection:false, not null, so the fixture
// does the same.
func apiCollectionRowJSON(key, name, parentKey string) string {
	parent := "false"
	if parentKey != "" {
		parent = fmt.Sprintf("%q", parentKey)
	}
	return fmt.Sprintf(`{"key":%q,"version":1,"library":{"type":"user","id":0},"data":{"key":%q,"version":1,"name":%q,"parentCollection":%s}}`,
		key, key, name, parent)
}

// TestAPICollectionPathPaginatesCollections proves the resolver keeps paging
// past the first full page of /collections. A collection that lives on page
// two must still resolve, and its path must be joined to a parent that lives
// on page one.
func TestAPICollectionPathPaginatesCollections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := newConnectorTargetFake(t)
	firstPage := make([]string, 0, 100)
	for i := range 99 {
		firstPage = append(firstPage, apiCollectionRowJSON(fmt.Sprintf("FILL%02d", i), fmt.Sprintf("Filler %02d", i), ""))
	}
	firstPage = append(firstPage, apiCollectionRowJSON("TOPKEY", "Top", ""))
	fake.collectionPages = [][]string{
		firstPage,
		{apiCollectionRowJSON("CHILDKEY", "Child", "TOPKEY")},
	}

	got, err := apiCollectionPath(fake.flags(t, ""), "CHILDKEY")
	if err != nil {
		t.Fatalf("apiCollectionPath returned error %v, want path %q", err, "Top/Child")
	}
	if got != "Top/Child" {
		t.Fatalf("path = %q, want %q", got, "Top/Child")
	}
	if n := fake.collectionRequests.Load(); n != 2 {
		t.Fatalf("collection requests = %d, want 2 (one full page plus the short final page)", n)
	}
}

// TestAPICollectionPathFailures pins the arms where the Web API cannot supply a
// trustworthy path. Every one must return an error, because a silent empty path
// would file the write into the desktop root.
func TestAPICollectionPathFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name    string
		setup   func(fake *connectorTargetFake)
		key     string
		wantErr string
	}{
		{
			name: "absent collection key refuses to invent a path",
			setup: func(fake *connectorTargetFake) {
				fake.collectionPages = [][]string{{apiCollectionRowJSON("TOPKEY", "Top", "")}}
			},
			key:     "GHOSTKEY",
			wantErr: `collection key GHOSTKEY was not found in the live Zotero collection list`,
		},
		{
			name: "non-2xx collection listing fails closed",
			setup: func(fake *connectorTargetFake) {
				fake.collectionsStatus = http.StatusForbidden
			},
			key:     "CHILDKEY",
			wantErr: "403",
		},
		{
			name: "undecodable collection listing fails closed",
			setup: func(fake *connectorTargetFake) {
				fake.collectionsBody = `{"collections":"not a list"}`
			},
			key:     "CHILDKEY",
			wantErr: "decoding collections for connector target resolution",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newConnectorTargetFake(t)
			tc.setup(fake)
			got, err := apiCollectionPath(fake.flags(t, ""), tc.key)
			if err == nil {
				t.Fatalf("apiCollectionPath = %q, want error containing %q", got, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
			if got != "" {
				t.Fatalf("path = %q, want empty on failure", got)
			}
		})
	}
}

// TestResolveConnectorTargetRouting pins every write-routing outcome of
// resolveConnectorTarget and resolveConnectorTargetForItem: which desktop
// target a Web API collection key selects, and when the resolver must refuse
// to select one at all.
func TestResolveConnectorTargetRouting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	nestedTree := []fakeConnectorTarget{
		{id: "L1", name: "My Library", level: 0},
		{id: "C1", name: "Top", level: 1},
		{id: "C2", name: "Child", level: 2},
		{id: "C3", name: "Sibling", level: 1},
	}
	ambiguousTree := []fakeConnectorTarget{
		{id: "L1", name: "My Library", level: 0},
		{id: "C1", name: "Top", level: 1},
		{id: "C2", name: "Child", level: 2},
		{id: "C3", name: "Top", level: 1},
		{id: "C4", name: "Child", level: 2},
	}
	unrelatedTree := []fakeConnectorTarget{
		{id: "L1", name: "My Library", level: 0},
		{id: "C9", name: "Somewhere Else", level: 1},
	}
	collections := []string{
		apiCollectionRowJSON("TOPKEY", "Top", ""),
		apiCollectionRowJSON("CHILDKEY", "Child", "TOPKEY"),
	}

	cases := []struct {
		name string
		// viaItem selects resolveConnectorTargetForItem over resolveConnectorTarget.
		viaItem             bool
		item                map[string]any
		collectionRequested bool
		collectionKey       string
		override            string
		targets             []fakeConnectorTarget
		wantTarget          string
		wantErr             string
		// wantErrExact pins the whole diagnostic, including the remediation
		// suffix, for arms whose message is itself the contract.
		wantErrExact       string
		wantCollectionReqs int64
		wantConnectorReqs  int64
	}{
		{
			name:                "explicit --connector-target wins and queries nothing",
			viaItem:             true,
			item:                map[string]any{"collections": []any{"CHILDKEY"}},
			collectionRequested: true,
			override:            "C78",
			targets:             nestedTree,
			wantTarget:          "C78",
		},
		{
			name:       "no collection requested resolves no target",
			viaItem:    true,
			item:       map[string]any{"collections": []any{"CHILDKEY"}},
			targets:    nestedTree,
			wantTarget: "",
		},
		{
			name:                "collection requested without a key on the item fails closed",
			viaItem:             true,
			item:                map[string]any{"title": "No collections"},
			collectionRequested: true,
			targets:             nestedTree,
			wantErr:             "--collection was requested but no collection key was present on the item",
		},
		{
			name:               "unknown collection key fails before the desktop is asked",
			collectionKey:      "GHOSTKEY",
			targets:            nestedTree,
			wantErr:            "collection key GHOSTKEY was not found in the live Zotero collection list",
			wantCollectionReqs: 1,
		},
		{
			name:               "single matching desktop path selects that target",
			collectionKey:      "CHILDKEY",
			targets:            nestedTree,
			wantTarget:         "C2",
			wantCollectionReqs: 1,
			wantConnectorReqs:  1,
		},
		{
			name:                "item collection key routes through the desktop tree",
			viaItem:             true,
			item:                map[string]any{"collections": []any{"CHILDKEY"}},
			collectionRequested: true,
			targets:             nestedTree,
			wantTarget:          "C2",
			wantCollectionReqs:  1,
			wantConnectorReqs:   1,
		},
		{
			name:               "no matching desktop path refuses to file the write",
			collectionKey:      "CHILDKEY",
			targets:            unrelatedTree,
			wantErr:            `collection CHILDKEY maps to path "Top/Child", but no desktop connector target matched it`,
			wantCollectionReqs: 1,
			wantConnectorReqs:  1,
		},
		{
			name:               "two desktop paths matching one collection fail closed",
			collectionKey:      "CHILDKEY",
			targets:            ambiguousTree,
			wantErrExact:       `collection CHILDKEY maps to ambiguous connector path "Top/Child" (C2, C4); pass --connector-target C<n>`,
			wantCollectionReqs: 1,
			wantConnectorReqs:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newConnectorTargetFake(t)
			fake.collectionPages = [][]string{collections}
			fake.targets = tc.targets
			flags := fake.flags(t, tc.override)

			var (
				got string
				err error
			)
			if tc.viaItem {
				got, err = resolveConnectorTargetForItem(context.Background(), flags, fake.connectorClient(), tc.item, tc.collectionRequested)
			} else {
				got, err = resolveConnectorTarget(context.Background(), flags, fake.connectorClient(), tc.collectionKey)
			}

			switch {
			case tc.wantErrExact != "":
				if err == nil {
					t.Fatalf("resolve returned target %q, want error %q", got, tc.wantErrExact)
				}
				if err.Error() != tc.wantErrExact {
					t.Fatalf("error = %q, want exactly %q", err.Error(), tc.wantErrExact)
				}
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("resolve returned target %q, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("resolve returned error %v, want target %q", err, tc.wantTarget)
				}
			}
			if got != tc.wantTarget {
				t.Fatalf("target = %q, want %q", got, tc.wantTarget)
			}
			if n := fake.collectionRequests.Load(); n != tc.wantCollectionReqs {
				t.Fatalf("collection requests = %d, want %d", n, tc.wantCollectionReqs)
			}
			if n := fake.connectorRequests.Load(); n != tc.wantConnectorReqs {
				t.Fatalf("connector requests = %d, want %d", n, tc.wantConnectorReqs)
			}
		})
	}
}
