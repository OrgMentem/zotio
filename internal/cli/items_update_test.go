// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

func TestItemsUpdateAbortsWhenVersionReadFails(t *testing.T) {
	fastRetryBackoff(t)
	patchIssued := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			http.Error(w, "version service unavailable", http.StatusServiceUnavailable)
		case http.MethodPatch:
			patchIssued = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K", "--title", "updated"})
	err := cmd.Execute()
	if ExitCode(err) != 5 {
		t.Fatalf("ExitCode(update error) = %d, want 5; err = %v", ExitCode(err), err)
	}
	if patchIssued {
		t.Fatal("PATCH issued after failed version read")
	}
}

// replacePathParam already percent-encodes the key; pre-escaping it here would
// double-encode path metacharacters and target a different resource.
func TestItemsUpdateEscapesItemKeyExactlyOnce(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "3")
			_, _ = w.Write([]byte(`{"key":"A/B","version":3,"data":{"key":"A/B","version":3}}`))
		case http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"A/B", "--title", "updated"})
	_ = cmd.Execute()

	if want := "/users/0/items/A%2FB"; gotPath != want {
		t.Fatalf("request path = %q, want %q (single escape)", gotPath, want)
	}
}
func TestItemsUpdateStdinUsesMutationEnvelopeAndJournalID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "11")
			_, _ = w.Write([]byte(`{}`))
		case http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	cmd := newItemsUpdateCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"ITEM1", "--stdin"})
	cmd.SetIn(strings.NewReader(`{"title":"updated"}`))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items update --stdin: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode mutation envelope: %v (output=%s)", err, out.String())
	}
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Key != "ITEM1" {
		t.Fatalf("result = %+v, want result.items[0].key ITEM1", env.Result)
	}
	journal, ok := env.Journal.(map[string]any)
	if !ok || journal["run_id"] == nil || journal["run_id"] == "" {
		t.Fatalf("journal = %#v, want run_id", env.Journal)
	}
}

func TestItemsUpdateUsesApplyTimeWritePlaneVersion(t *testing.T) {
	var getCount int
	var patchHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCount++
			ver := "5"
			if getCount == 2 {
				ver = "9"
			}
			w.Header().Set("Last-Modified-Version", ver)
			_, _ = w.Write([]byte(`{"key":"K","version":` + ver + `,"data":{"key":"K","version":` + ver + `}}`))
		case http.MethodPatch:
			patchHeader = r.Header.Get("If-Unmodified-Since-Version")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K", "--title", "updated"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items update: %v (output %s)", err, out.String())
	}
	if patchHeader != "9" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want %q (apply-time); getCount=%d", patchHeader, "9", getCount)
	}
	if getCount != 2 {
		t.Fatalf("GET count = %d, want 2 (plan + apply)", getCount)
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (output %s)", err, out.String())
	}
	if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].ExpectedVersion != 5 {
		t.Fatalf("plan ExpectedVersion = %v, want 5 (plan-time)", env.Plan.Operations[0].ExpectedVersion)
	}
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("result = %+v, want single applied item", env.Result)
	}
}

func TestItemsUpdateMapsPreconditionFailedToConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "7")
			_, _ = w.Write([]byte(`{"key":"K","version":7,"data":{"key":"K","version":7}}`))
		case http.MethodPatch:
			http.Error(w, "stale version", http.StatusPreconditionFailed)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	cmd := newItemsUpdateCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K", "--title", "updated"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("items update with 412 = nil error, want failure")
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (output %s) err %v", err, out.String(), err)
	}
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("result = %+v, want one item", env.Result)
	}
	if got := env.Result.Items[0].Status; got != "conflict" {
		t.Fatalf("result status = %q, want %q (412 must map to conflict, not generic failed); reason=%v", got, "conflict", env.Result.Items[0].Reason)
	}
}

func TestItemsUpdateMapsPreconditionRequiredToConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "7")
			_, _ = w.Write([]byte(`{"key":"K","version":7,"data":{"key":"K","version":7}}`))
		case http.MethodPatch:
			http.Error(w, "missing precondition", http.StatusPreconditionRequired)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K", "--title", "updated"})
	_ = cmd.Execute()
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Result == nil || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("result status = %v, want conflict for 428; envelope=%s", env.Result, out.String())
	}
}

// itemsUpdateArgGuardServer counts every HTTP request so a refused argument
// list can prove that no network call was attempted, and answers the version
// GET an apply-mode run needs.
type itemsUpdateArgGuardServer struct {
	server   *httptest.Server
	requests int
}

func newItemsUpdateArgGuardServer(t *testing.T) *itemsUpdateArgGuardServer {
	t.Helper()
	g := &itemsUpdateArgGuardServer{}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests++
		w.Header().Set("Last-Modified-Version", "3")
		_, _ = w.Write([]byte(`{"key":"K1","version":3,"data":{"key":"K1","version":3}}`))
	}))
	t.Cleanup(g.server.Close)
	return g
}
func runItemsUpdateArgGuardCmd(t *testing.T, g *itemsUpdateArgGuardServer, cmd interface {
	SetOut(io.Writer)
	SetErr(io.Writer)
	SetArgs([]string)
	Execute() error
}, args ...string) error {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", g.server.URL+"/users/0")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

// `items update K1 K2` updated K1 and dropped K2 with no mention of it: the
// request path, the op id and the whole envelope are built from args[0] alone.
// The refusal must come before any network call.
func TestItemsUpdateRefusesMoreThanOneKey(t *testing.T) {
	g := newItemsUpdateArgGuardServer(t)

	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := runItemsUpdateArgGuardCmd(t, g, cmd, "K1", "K2", "--title", "updated")
	if err == nil {
		t.Fatal("items update accepted two keys; it acts on the first and drops the rest without saying so")
	}
	if !strings.Contains(err.Error(), "accepts at most 1 arg(s)") {
		t.Errorf("error = %q, want the arity bound the neighbouring commands report", err.Error())
	}
	// The sibling single-target commands surface this refusal as exit 1 (a
	// standalone Cobra arity error, not a wrapped usage error); the update
	// command must match that convention, not invent its own code.
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1 (the Cobra-arity convention siblings follow)", ExitCode(err))
	}
	if g.requests != 0 {
		t.Errorf("requests = %d, want 0: a refused argument list must not reach the library", g.requests)
	}

	// Zero args still renders help rather than erroring, which is why the
	// bound is MaximumNArgs and not ExactArgs.
	helpGuard := newItemsUpdateArgGuardServer(t)
	help := newItemsUpdateCmd(&rootFlags{asJSON: true})
	help.SilenceErrors, help.SilenceUsage = true, true
	if err := runItemsUpdateArgGuardCmd(t, helpGuard, help); err != nil {
		t.Errorf("items update with no key = %v, want the help output", err)
	}

	// The single-key path still previews unchanged: no error and no HTTP call.
	singleGuard := newItemsUpdateArgGuardServer(t)
	single := newItemsUpdateCmd(&rootFlags{asJSON: true, maxChanges: -1})
	single.SilenceErrors, single.SilenceUsage = true, true
	if err := runItemsUpdateArgGuardCmd(t, singleGuard, single, "K1", "--title", "updated"); err != nil {
		t.Fatalf("items update with one key = %v, want the preview envelope", err)
	}
	if singleGuard.requests != 0 {
		t.Errorf("requests = %d, want 0: the preview must not reach the library", singleGuard.requests)
	}
}

// `items restore K1 K2` restored K1 and dropped K2 with no mention of it, the
// same silent-partial-action shape as update. Same guard, same convention.
func TestItemsRestoreRefusesMoreThanOneKey(t *testing.T) {
	g := newItemsUpdateArgGuardServer(t)

	cmd := newItemsRestoreCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := runItemsUpdateArgGuardCmd(t, g, cmd, "K1", "K2")
	if err == nil {
		t.Fatal("items restore accepted two keys; it acts on the first and drops the rest without saying so")
	}
	if !strings.Contains(err.Error(), "accepts at most 1 arg(s)") {
		t.Errorf("error = %q, want the arity bound the neighbouring commands report", err.Error())
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1 (the Cobra-arity convention siblings follow)", ExitCode(err))
	}
	if g.requests != 0 {
		t.Errorf("requests = %d, want 0: a refused argument list must not reach the library", g.requests)
	}

	// Zero args still renders help rather than erroring.
	helpGuard := newItemsUpdateArgGuardServer(t)
	help := newItemsRestoreCmd(&rootFlags{asJSON: true})
	help.SilenceErrors, help.SilenceUsage = true, true
	if err := runItemsUpdateArgGuardCmd(t, helpGuard, help); err != nil {
		t.Errorf("items restore with no key = %v, want the help output", err)
	}

	// The single-key path still previews unchanged: no error and no HTTP call.
	singleGuard := newItemsUpdateArgGuardServer(t)
	single := newItemsRestoreCmd(&rootFlags{asJSON: true, maxChanges: -1})
	single.SilenceErrors, single.SilenceUsage = true, true
	if err := runItemsUpdateArgGuardCmd(t, singleGuard, single, "K1"); err != nil {
		t.Fatalf("items restore with one key = %v, want the preview envelope", err)
	}
	if singleGuard.requests != 0 {
		t.Errorf("requests = %d, want 0: the preview must not reach the library", singleGuard.requests)
	}
}

// itemsUpdateWritePlaneServer answers the version GET and the PATCH an
// apply-mode update needs, recording PATCH bodies for assertion.
type itemsUpdateWritePlaneServer struct {
	server      *httptest.Server
	patchBodies []string
}

func newItemsUpdateWritePlaneServer(t *testing.T) *itemsUpdateWritePlaneServer {
	t.Helper()
	s := &itemsUpdateWritePlaneServer{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "5")
			_, _ = w.Write([]byte(`{"key":"K1","version":5,"data":{"key":"K1","version":5}}`))
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			s.patchBodies = append(s.patchBodies, string(body))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

// itemsUpdateSeedMirrorRow writes one raw item row into the mirror at dbPath,
// proving later assertions against the store rather than a mock.
func itemsUpdateSeedMirrorRow(t *testing.T, dbPath, raw string) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{json.RawMessage(raw)}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
}

func itemsUpdateMirroredTags(t *testing.T, dbPath, key string) []map[string]any {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	rows, err := (localQueryStore{db}).QueryRaw(
		"SELECT json_extract(data,'$.data.tags') AS tags FROM resources WHERE resource_type='items' AND id=?", key)
	if err != nil {
		t.Fatalf("read back mirror: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mirror rows for %s = %d, want 1", key, len(rows))
	}
	raw := sqlStringValue(rows[0]["tags"])
	if raw == "" || raw == "null" {
		return nil
	}
	var tags []map[string]any
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		t.Fatalf("decode mirrored tags %q: %v", raw, err)
	}
	return tags
}

func itemsUpdateMirroredCollections(t *testing.T, dbPath, key string) []string {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	rows, err := (localQueryStore{db}).QueryRaw(
		"SELECT json_extract(data,'$.data.collections') AS cols FROM resources WHERE resource_type='items' AND id=?", key)
	if err != nil {
		t.Fatalf("read back mirror: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mirror rows for %s = %d, want 1", key, len(rows))
	}
	raw := sqlStringValue(rows[0]["cols"])
	if raw == "" || raw == "null" {
		return nil
	}
	var cols []string
	if err := json.Unmarshal([]byte(raw), &cols); err != nil {
		t.Fatalf("decode mirrored collections %q: %v", raw, err)
	}
	return cols
}

func itemsUpdatePendingMarkerPresent(t *testing.T, dbPath, key string) bool {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("read pending writes: %v", err)
	}
	_, ok := pending[key]
	return ok
}

// A tag replacement must land in the mirror with no intervening sync: before
// this fix the "tags_set" change fell through applyChangeToItemData, so the
// write reported success while local reads kept serving the pre-write tags.
func TestItemsUpdateTagsReplacementWritesThroughToLocalMirror(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	itemsUpdateSeedMirrorRow(t, dbPath,
		`{"key":"K1","version":24,"data":{"key":"K1","itemType":"journalArticle","title":"Paper","tags":[{"tag":"old"}],"collections":[]}}`)

	// Install the hook exactly as Execute does.
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = nil })

	srv := newItemsUpdateWritePlaneServer(t)
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K1", "--tags", `[{"tag":"new"}]`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items update --tags: %v (output %s)", err, out.String())
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (output %s)", err, out.String())
	}
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("result = %+v, want one applied update", env.Result)
	}

	// The mirror must reflect the replacement with no intervening sync.
	tags := itemsUpdateMirroredTags(t, dbPath, "K1")
	if len(tags) != 1 || tags[0]["tag"] != "new" {
		t.Fatalf("mirror tags after update = %v, want [{tag:new}]", tags)
	}

	// And the write must leave a pending-write marker: without one the next
	// sync re-applies the pre-write copy over the mirror and rolls the
	// replacement back for every later read.
	if !itemsUpdatePendingMarkerPresent(t, dbPath, "K1") {
		t.Fatal("no pending-write marker for K1: a sync inside the plane-lag window would roll the tag replacement back")
	}

	// The envelope must carry the post-write state so an agent needs no
	// follow-up read at all.
	if env.Result.Items[0].Item == nil {
		t.Fatal("applied result item carries no post-write state")
	}
	data, _ := env.Result.Items[0].Item["data"].(map[string]any)
	envTags, _ := data["tags"].([]any)
	if len(envTags) != 1 {
		t.Fatalf("envelope post-write tags = %v, want one tag", data["tags"])
	}
	if m, ok := envTags[0].(map[string]any); !ok || m["tag"] != "new" {
		t.Fatalf("envelope post-write tags = %v, want [{tag:new}]", data["tags"])
	}
}

// The reporter's exact sequence for the mirror defect: update collections,
// then sync from a read plane that has not received the Web API write yet.
// Without pending-write reconciliation the sync rolled the replacement back;
// the marker must hold the mirror on the replacement until the read plane
// matches it.
func TestItemsUpdateCollectionsReplacementSurvivesStaleSync(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	itemsUpdateSeedMirrorRow(t, dbPath,
		`{"key":"K1","version":24,"data":{"key":"K1","itemType":"journalArticle","title":"Paper","tags":[],"collections":["OLD"]}}`)

	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = nil })

	srv := newItemsUpdateWritePlaneServer(t)
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K1", "--collections", `["NEW"]`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items update --collections: %v", err)
	}

	if cols := itemsUpdateMirroredCollections(t, dbPath, "K1"); len(cols) != 1 || cols[0] != "NEW" {
		t.Fatalf("mirror collections after update = %v, want [NEW]", cols)
	}

	// Now sync from a read plane that still reports the pre-write membership
	// at a newer version, the plane-lag window the finding describes.
	readPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","version":25,"data":{"key":"K1","itemType":"journalArticle","title":"Paper","tags":[],"collections":["OLD"]}}]`))
	}))
	defer readPlane.Close()

	syncDB, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store for sync: %v", err)
	}
	if res := syncResource(context.Background(), syncTestClient(readPlane.URL), syncDB, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("sync error: %v", res.Err)
	}
	if err := syncDB.Close(); err != nil {
		t.Fatalf("close sync store: %v", err)
	}

	if cols := itemsUpdateMirroredCollections(t, dbPath, "K1"); len(cols) != 1 || cols[0] != "NEW" {
		t.Fatalf("collections after update+stale-sync = %v, want [NEW]: the sync rolled the replacement back", cols)
	}
	// The marker must still stand: the read plane has not matched the write,
	// so clearing it now would expose the next sync to the same rollback.
	if !itemsUpdatePendingMarkerPresent(t, dbPath, "K1") {
		t.Fatal("pending-write marker cleared by a stale sync: the next sync would roll the replacement back")
	}
}
