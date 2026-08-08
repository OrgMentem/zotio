// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// A tag rename must take its write precondition from the write plane.

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// The local API reports an empty version (and empty Last-Modified-Version) for
// items it has never pushed upstream, and its version space is unrelated to the
// web API's anyway. Trusting the plan-time version meant no
// If-Unmodified-Since-Version header at all, which Zotero rejects outright, so
// every rename failed. The precondition must come from the write plane.
func TestApplyTagRenameUpdateTakesVersionFromWritePlane(t *testing.T) {
	readServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("apply read the local plane at %s; the precondition must come from the write plane", r.URL.Path)
		http.Error(w, "wrong plane", http.StatusInternalServerError)
	}))
	defer readServer.Close()

	var gotPrecondition string
	var gotBody map[string]any
	patches := 0
	writeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"K1","version":12704,"data":{"key":"K1","tags":[{"tag":"depression","type":0},{"tag":"keep","type":1}]}}`))
		case http.MethodPatch:
			patches++
			gotPrecondition = r.Header.Get("If-Unmodified-Since-Version")
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode patch body: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer writeServer.Close()

	c := client.New(&config.Config{BaseURL: readServer.URL + "/users/0"}, 5*time.Second, 0)
	c.NoCache = true
	c.ResolveWriteBase = func(context.Context) (string, error) { return writeServer.URL + "/users/0", nil }

	// version is what the local plane reported: the empty string Zotero returns
	// for an item that has never synced upstream.
	update := tagRenameUpdate{key: "K1", oldTag: "depression", newTag: "Depression", version: ""}
	status, reason, err := applyTagRenameUpdate(c, update)
	if err != nil {
		t.Fatalf("applyTagRenameUpdate: %v (reason %v)", err, reason)
	}
	if status != "applied" {
		t.Fatalf("status = %q (reason %v), want applied", status, reason)
	}
	if patches != 1 {
		t.Fatalf("PATCH count = %d, want 1", patches)
	}
	if gotPrecondition != "12704" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want the write plane's version 12704", gotPrecondition)
	}

	tags, ok := gotBody["tags"].([]any)
	if !ok {
		t.Fatalf("patch body tags = %#v, want an array", gotBody["tags"])
	}
	// The tag list must be recomputed from the write plane's own copy, not the
	// plan-time copy, or the PATCH overwrites upstream state with stale data.
	names := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tag entry = %#v", raw)
		}
		names = append(names, tag["tag"].(string))
	}
	if len(names) != 2 || names[0] != "Depression" || names[1] != "keep" {
		t.Fatalf("patched tags = %v, want [Depression keep]", names)
	}
}

// When the write plane no longer carries the old tag there is nothing to do, and
// the result must say so rather than reporting a bare no_op.
func TestApplyTagRenameUpdateReportsAbsentTag(t *testing.T) {
	writeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			t.Error("patched an item that no longer carries the tag")
			http.Error(w, "unexpected patch", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"K1","version":12704,"data":{"key":"K1","tags":[{"tag":"Depression","type":0}]}}`))
	}))
	defer writeServer.Close()

	c := client.New(&config.Config{BaseURL: writeServer.URL + "/users/0"}, 5*time.Second, 0)
	c.NoCache = true

	status, reason, err := applyTagRenameUpdate(c, tagRenameUpdate{key: "K1", oldTag: "depression", newTag: "Depression", version: 12704})
	if err != nil {
		t.Fatalf("applyTagRenameUpdate: %v", err)
	}
	if status != "no_op" {
		t.Fatalf("status = %q, want no_op", status)
	}
	detail, ok := reason.(map[string]any)
	if !ok || detail["code"] != "tag_absent" {
		t.Fatalf("reason = %#v, want a tag_absent code", reason)
	}
}

// New-1: write-through strips the stale version from rows it replays, so an item
// zotio has just written has no mirror version. Requiring one made `tags rename`
// report selected: 0 — indistinguishable from "no such tag" — and aborted a whole
// `tags audit fix` batch on the first such row. The write precondition comes from
// the write plane, so a version-less row must still be planned.
func TestBuildTagRenameUpdatesAcceptsVersionlessRows(t *testing.T) {
	// No "version" field, exactly as write-through leaves it.
	page := []byte(`[{"key":"ZFTIBMSY","data":{"key":"ZFTIBMSY","tags":[{"tag":"probe-b","type":0}]}}]`)

	updates, err := buildTagRenameUpdates(page, "probe-b", "Probe-B")
	if err != nil {
		t.Fatalf("buildTagRenameUpdates rejected a version-less row: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("selected %d items, want 1: a freshly written item is invisible to renames", len(updates))
	}
	if updates[0].key != "ZFTIBMSY" {
		t.Errorf("key = %q, want ZFTIBMSY", updates[0].key)
	}
	if mutationExpectedVersion(updates[0].version) != 0 {
		t.Errorf("version = %v, want 0 recorded for a row that has none", updates[0].version)
	}
}

// A version-less row must still rename end to end, taking its precondition from
// the write plane.
func TestApplyTagRenameUpdateWorksForVersionlessRow(t *testing.T) {
	var gotPrecondition string
	writeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"ZFTIBMSY","version":12750,"data":{"key":"ZFTIBMSY","tags":[{"tag":"probe-b","type":0}]}}`))
		case http.MethodPatch:
			gotPrecondition = r.Header.Get("If-Unmodified-Since-Version")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer writeServer.Close()

	c := client.New(&config.Config{BaseURL: writeServer.URL + "/users/0"}, 5*time.Second, 0)
	c.NoCache = true

	// version is nil: the mirror row had none.
	status, reason, err := applyTagRenameUpdate(c, tagRenameUpdate{key: "ZFTIBMSY", oldTag: "probe-b", newTag: "Probe-B"})
	if err != nil {
		t.Fatalf("applyTagRenameUpdate: %v (reason %v)", err, reason)
	}
	if status != "applied" {
		t.Fatalf("status = %q (reason %v), want applied", status, reason)
	}
	if gotPrecondition != "12750" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want 12750 from the write plane", gotPrecondition)
	}
}
