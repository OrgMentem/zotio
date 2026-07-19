// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

type creatorAuditFixPatch struct {
	Body   map[string]any
	Header string
}

type creatorAuditFixServer struct {
	server   *httptest.Server
	requests int
	patches  map[string]creatorAuditFixPatch
}

func newCreatorAuditFixServer(t *testing.T) *creatorAuditFixServer {
	t.Helper()
	ts := &creatorAuditFixServer{patches: map[string]creatorAuditFixPatch{}}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.requests++
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/users/0/items/") {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
		ts.patches[key] = creatorAuditFixPatch{Body: body, Header: r.Header.Get("If-Unmodified-Since-Version")}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func seedCreatorAuditFixStore(t *testing.T, items []json.RawMessage) {
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
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func creatorAuditFixFixtureItems() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","version":1,"itemType":"journalArticle","title":"Canonical","creators":[{"creatorType":"author","firstName":"John","lastName":"Smith"}]}}`),
		json.RawMessage(`{"key":"K2","version":2,"data":{"key":"K2","version":2,"itemType":"journalArticle","title":"Exact case drift","creators":[{"creatorType":"author","firstName":"John","lastName":"Smith"},{"creatorType":"author","firstName":"Alice","lastName":"Jones"},{"creatorType":"editor","firstName":"john","lastName":"smith"}]}}`),
		json.RawMessage(`{"key":"K3","version":3,"data":{"key":"K3","version":3,"itemType":"journalArticle","title":"Initial variant","creators":[{"creatorType":"author","firstName":"J.","lastName":"Smith"},{"creatorType":"author","firstName":"Carol","lastName":"Ng"}]}}`),
		json.RawMessage(`{"key":"K4","version":4,"data":{"key":"K4","version":4,"itemType":"journalArticle","title":"Bare surname","creators":[{"creatorType":"author","lastName":"Smith"}]}}`),
	}
}

func runCreatorAuditFixCmd(t *testing.T, flags *rootFlags, baseURL string, args ...string) (mutation.Envelope, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	cmd := newCreatorsAuditCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"fix"}, args...))
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, out.String(), err
}

func TestCreatorsAuditFixPreviewDefaultsToTier1OnlyWithoutWrites(t *testing.T) {
	seedCreatorAuditFixStore(t, creatorAuditFixFixtureItems())
	srv := newCreatorAuditFixServer(t)

	env, _, err := runCreatorAuditFixCmd(t, &rootFlags{asJSON: true}, srv.server.URL)
	if err != nil {
		t.Fatalf("creators audit fix preview: %v", err)
	}
	if srv.requests != 0 {
		t.Fatalf("preview made %d Zotero request(s), want 0", srv.requests)
	}
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "default" || env.Result != nil {
		t.Fatalf("preview envelope = %+v, want ok default preview without result", env)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("planned ops = summary %+v len %d, want exactly K2", env.Plan.Summary, len(env.Plan.Operations))
	}
	op := env.Plan.Operations[0]
	if op.Key != "K2" || op.Kind != "creator_rename" || op.Destructive {
		t.Fatalf("op = %+v, want non-destructive creator_rename for K2", op)
	}
	assertCreatorChange(t, op.Changes, "john smith", "John Smith")
}

func TestCreatorsAuditFixExplicitMapPlansTier2(t *testing.T) {
	seedCreatorAuditFixStore(t, creatorAuditFixFixtureItems())
	srv := newCreatorAuditFixServer(t)

	env, _, err := runCreatorAuditFixCmd(t, &rootFlags{asJSON: true}, srv.server.URL, "--map", "J. Smith=John Smith")
	if err != nil {
		t.Fatalf("creators audit fix mapped preview: %v", err)
	}
	if srv.requests != 0 {
		t.Fatalf("mapped preview made %d Zotero request(s), want 0", srv.requests)
	}
	if got := plannedCreatorKeys(env); !reflect.DeepEqual(got, []string{"K2", "K3"}) {
		t.Fatalf("planned keys = %v, want [K2 K3] (tier-3 K4 must stay unplanned)", got)
	}
	for _, op := range env.Plan.Operations {
		switch op.Key {
		case "K2":
			assertCreatorChange(t, op.Changes, "john smith", "John Smith")
		case "K3":
			assertCreatorChange(t, op.Changes, "J. Smith", "John Smith")
		default:
			t.Fatalf("unexpected planned op = %+v", op)
		}
	}
}

func TestCreatorsAuditFixApplyPatchesFullCreatorsAndWritesThrough(t *testing.T) {
	seedCreatorAuditFixStore(t, creatorAuditFixFixtureItems())
	srv := newCreatorAuditFixServer(t)
	oldMirror := mirrorWriteThrough
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = oldMirror })

	env, _, err := runCreatorAuditFixCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, srv.server.URL, "--map", "J. Smith=John Smith")
	if err != nil {
		t.Fatalf("creators audit fix apply: %v", err)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("apply envelope = %+v, want two applied writes", env)
	}
	if got := plannedCreatorKeys(env); !reflect.DeepEqual(got, []string{"K2", "K3"}) {
		t.Fatalf("planned keys = %v, want [K2 K3]", got)
	}
	if len(srv.patches) != 2 {
		t.Fatalf("patches = %#v, want K2 and K3", srv.patches)
	}

	assertCreatorPatch(t, srv.patches["K2"], "2", []map[string]string{
		{"creatorType": "author", "firstName": "John", "lastName": "Smith"},
		{"creatorType": "author", "firstName": "Alice", "lastName": "Jones"},
		{"creatorType": "editor", "firstName": "John", "lastName": "Smith"},
	})
	assertCreatorPatch(t, srv.patches["K3"], "3", []map[string]string{
		{"creatorType": "author", "firstName": "John", "lastName": "Smith"},
		{"creatorType": "author", "firstName": "Carol", "lastName": "Ng"},
	})
	assertStoredCreators(t, "K2", []map[string]string{
		{"creatorType": "author", "firstName": "John", "lastName": "Smith"},
		{"creatorType": "author", "firstName": "Alice", "lastName": "Jones"},
		{"creatorType": "editor", "firstName": "John", "lastName": "Smith"},
	})
	assertStoredCreators(t, "K3", []map[string]string{
		{"creatorType": "author", "firstName": "John", "lastName": "Smith"},
		{"creatorType": "author", "firstName": "Carol", "lastName": "Ng"},
	})
	assertStoredCreators(t, "K4", []map[string]string{
		{"creatorType": "author", "lastName": "Smith"},
	})
}

func TestCreatorsAuditFixMapAliasOutsideTier2IsUsageError(t *testing.T) {
	seedCreatorAuditFixStore(t, creatorAuditFixFixtureItems())
	srv := newCreatorAuditFixServer(t)

	_, _, err := runCreatorAuditFixCmd(t, &rootFlags{asJSON: true}, srv.server.URL, "--map", "Smith=John Smith")
	if err == nil {
		t.Fatal("creators audit fix accepted --map alias outside tier-2 group, want usage error")
	}
	if code := ExitCode(err); code != 2 {
		t.Fatalf("ExitCode(err) = %d (%v), want usage exit 2", code, err)
	}
	if !strings.Contains(err.Error(), `--map alias "Smith" was not found in any tier-2`) {
		t.Fatalf("err = %v, want tier-2 alias usage error", err)
	}
	if srv.requests != 0 {
		t.Fatalf("usage error made %d Zotero request(s), want 0", srv.requests)
	}
}

func TestCreatorsAuditFixMaxChangesCountsItemWrites(t *testing.T) {
	seedCreatorAuditFixStore(t, creatorAuditFixFixtureItems())
	srv := newCreatorAuditFixServer(t)

	refused, refusedJSON, err := runCreatorAuditFixCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: 1}, srv.server.URL, "--map", "J. Smith=John Smith")
	if err == nil {
		t.Fatal("creators audit fix apply succeeded, want max_changes_exceeded error")
	}
	if refused.OK || refused.Error == nil || refused.Error.Code != "max_changes_exceeded" {
		t.Fatalf("refused envelope = %+v, want max_changes_exceeded", refused)
	}
	if got := plannedCreatorKeys(refused); !reflect.DeepEqual(got, []string{"K2", "K3"}) || refused.Plan.Summary.Planned != 2 {
		t.Fatalf("refused plan keys=%v summary=%+v, want two planned item writes", got, refused.Plan.Summary)
	}
	if !bytes.Contains([]byte(refusedJSON), []byte(`"planned": 2`)) {
		t.Fatalf("refused JSON %q does not include planned item-write count 2", refusedJSON)
	}
	if srv.requests != 0 {
		t.Fatalf("refused apply made %d PATCH request(s), want 0", srv.requests)
	}

	seedCreatorAuditFixStore(t, creatorAuditFixFixtureItems())
	allowed, _, err := runCreatorAuditFixCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: 2}, srv.server.URL, "--map", "J. Smith=John Smith")
	if err != nil {
		t.Fatalf("creators audit fix apply at cap: %v", err)
	}
	if !allowed.OK || allowed.Result == nil || allowed.Plan.Summary.Planned != 2 || allowed.Result.Summary.Applied != 2 {
		t.Fatalf("allowed envelope = %+v, want two planned/applied item writes", allowed)
	}
	if srv.requests != 2 {
		t.Fatalf("PATCH requests after allowed apply = %d, want 2", srv.requests)
	}
}

func assertCreatorChange(t *testing.T, changes []mutation.Change, remove, add string) {
	t.Helper()
	if len(changes) != 1 || changes[0].Field != "creators" || changes[0].Remove != remove || changes[0].Add != add {
		t.Fatalf("changes = %+v, want creators rename %q -> %q", changes, remove, add)
	}
}

func plannedCreatorKeys(env mutation.Envelope) []string {
	keys := make([]string, 0, len(env.Plan.Operations))
	for _, op := range env.Plan.Operations {
		keys = append(keys, op.Key)
	}
	return keys
}

func assertCreatorPatch(t *testing.T, patch creatorAuditFixPatch, version string, want []map[string]string) {
	t.Helper()
	if patch.Body == nil {
		t.Fatalf("missing PATCH body for version %s", version)
	}
	if patch.Header != version {
		t.Fatalf("If-Unmodified-Since-Version = %q, want %q", patch.Header, version)
	}
	assertCreatorArray(t, patch.Body["creators"], want)
	if len(patch.Body) != 1 {
		t.Fatalf("PATCH body keys = %#v, want only full creators array", patch.Body)
	}
}

func assertStoredCreators(t *testing.T, key string, want []map[string]string) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	rows, err := (localQueryStore{db}).QueryRaw("SELECT data FROM resources WHERE resource_type='items' AND id=?", key)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read %s: rows=%v err=%v", key, rows, err)
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(sqlStringValue(rows[0]["data"])), &item); err != nil {
		t.Fatalf("decode stored item %s: %v", key, err)
	}
	data, ok := item["data"].(map[string]any)
	if !ok {
		t.Fatalf("stored item %s data = %#v", key, item["data"])
	}
	assertCreatorArray(t, data["creators"], want)
}

func assertCreatorArray(t *testing.T, raw any, want []map[string]string) {
	t.Helper()
	creators, ok := raw.([]any)
	if !ok {
		t.Fatalf("creators = %#v, want array", raw)
	}
	if len(creators) != len(want) {
		t.Fatalf("creators len = %d (%#v), want %d", len(creators), creators, len(want))
	}
	for i, rawCreator := range creators {
		creator, ok := rawCreator.(map[string]any)
		if !ok {
			t.Fatalf("creator[%d] = %#v, want object", i, rawCreator)
		}
		for field, wantValue := range want[i] {
			if got := fmt.Sprint(creator[field]); got != wantValue {
				t.Fatalf("creator[%d].%s = %q, want %q in %#v", i, field, got, wantValue, creators)
			}
		}
		for _, absent := range []string{"firstName", "lastName", "name"} {
			if _, expected := want[i][absent]; !expected {
				if got, exists := creator[absent]; exists && got != "" {
					t.Fatalf("creator[%d].%s = %q, want absent in %#v", i, absent, got, creators)
				}
			}
		}
	}
}
