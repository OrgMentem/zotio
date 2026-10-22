// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// totalsReply is one scripted answer of a fake read plane.
type totalsReply struct {
	body    string
	total   string // Total-Results; empty sends no header
	version string // Last-Modified-Version
}

// totalsServer answers every GET with whatever reply the test has scripted
// for the request's start offset, so one test can move the plane from a
// faulty response to a healthy one between runs.
func totalsServer(t *testing.T, script *atomic.Pointer[func(start string) totalsReply]) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := (*script.Load())(r.URL.Query().Get("start"))
		w.Header().Set("Content-Type", "application/json")
		if reply.total != "" {
			w.Header().Set("Total-Results", reply.total)
		}
		if reply.version != "" {
			w.Header().Set("Last-Modified-Version", reply.version)
		}
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func totalsConstant(reply totalsReply) *func(string) totalsReply {
	f := func(string) totalsReply { return reply }
	return &f
}

func totalsItems(keys ...string) string {
	rows := make([]string, len(keys))
	for i, k := range keys {
		rows[i] = string(pendingWritesTestItem(k))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// A full pass sweeps every mirrored row its listing omits and checkpoints the
// response's library version. A 200 whose array holds only A while
// Total-Results says 2 is a partial listing, not a library with one item:
// reading it as complete deleted B and checkpointed past it, and a healthy
// incremental sync (which never re-sends unchanged B) could not repair that.
func TestFullSyncRejectsTotalResultsMismatch(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	var script atomic.Pointer[func(string) totalsReply]
	srv := totalsServer(t, &script)
	c := syncTestClient(srv.URL)

	if _, err := db.UpsertKeyed("items", []string{"AAAA", "BBBB"}, []json.RawMessage{pendingWritesTestItem("AAAA"), pendingWritesTestItem("BBBB")}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.SaveLibraryVersion("items", c.Plane(), 10); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	assertIntact := func(t *testing.T, label string) {
		t.Helper()
		ids, err := db.ResourceIDs("items")
		if err != nil {
			t.Fatalf("ResourceIDs: %v", err)
		}
		if !ids["AAAA"] || !ids["BBBB"] {
			t.Errorf("%s: mirrored items = %v, want both AAAA and BBBB kept", label, ids)
		}
		if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "items"); v != 10 {
			t.Errorf("%s: library version = %d, want 10 (not advanced past an unverified listing)", label, v)
		}
	}

	for _, tc := range []struct {
		name  string
		reply totalsReply
	}{
		{name: "short array", reply: totalsReply{body: totalsItems("AAAA"), total: "2", version: "20"}},
		{name: "empty array with positive total", reply: totalsReply{body: `[]`, total: "2", version: "20"}},
		{name: "longer array than total", reply: totalsReply{body: totalsItems("AAAA", "BBBB"), total: "1", version: "20"}},
		{name: "malformed total", reply: totalsReply{body: totalsItems("AAAA", "BBBB"), total: "many", version: "20"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script.Store(totalsConstant(tc.reply))
			res := syncResource(context.Background(), c, db, "items", 0, true, 0, false)
			if res.Err == nil || !strings.Contains(res.Err.Error(), "Total-Results") {
				t.Fatalf("full sync err = %v, want a Total-Results refusal", res.Err)
			}
			assertIntact(t, tc.name)
		})
	}

	// The plane recovers: the same sync now succeeds and converges.
	script.Store(totalsConstant(totalsReply{body: totalsItems("AAAA", "BBBB"), total: "2", version: "20"}))
	if res := syncResource(context.Background(), c, db, "items", 0, true, 0, false); res.Err != nil {
		t.Fatalf("recovered full sync: %v", res.Err)
	}
	if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "items"); v != 20 {
		t.Errorf("library version after the healthy pass = %d, want 20", v)
	}

	// Control: a listing whose count agrees is still authoritative, so the
	// refusal above is not "never sweep".
	script.Store(totalsConstant(totalsReply{body: totalsItems("AAAA"), total: "1", version: "21"}))
	if res := syncResource(context.Background(), c, db, "items", 0, true, 0, false); res.Err != nil {
		t.Fatalf("control full sync: %v", res.Err)
	}
	ids, err := db.ResourceIDs("items")
	if err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	}
	if ids["BBBB"] || !ids["AAAA"] {
		t.Errorf("mirrored items after a verified listing of {AAAA} = %v, want BBBB reaped and AAAA kept", ids)
	}
}

// No Total-Results at all is missing evidence, not an empty library: a full
// pass must not sweep on it (the key-listing reconciliation applies the same
// rule), and must say so.
func TestFullSyncSkipsSweepWithoutTotalResults(t *testing.T) {
	syncTestWithHumanFriendly(t, true)
	db := syncTestOpenStore(t)
	defer db.Close()

	var script atomic.Pointer[func(string) totalsReply]
	script.Store(totalsConstant(totalsReply{body: `[]`, version: "20"}))
	srv := totalsServer(t, &script)

	if _, err := db.UpsertKeyed("items", []string{"BBBB"}, []json.RawMessage{pendingWritesTestItem("BBBB")}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	stderr := captureStderr(t, func() {
		if res := syncResource(context.Background(), syncTestClient(srv.URL), db, "items", 0, true, 0, false); res.Err != nil {
			t.Fatalf("full sync: %v", res.Err)
		}
	})
	if ids, err := db.ResourceIDs("items"); err != nil || !ids["BBBB"] {
		t.Errorf("mirrored items = %v (err %v), want BBBB kept: an array with no Total-Results is not proof of an empty library", ids, err)
	}
	if !strings.Contains(stderr, "Total-Results") {
		t.Errorf("stderr = %q, want a warning that the sweep was skipped for lack of Total-Results", stderr)
	}
}

// The global schema listings are one unpaginated response that Zotero sends
// without Total-Results, so the count rule does not apply to them: a full
// pass still reaps a field Zotero no longer lists, and raises no anomaly.
func TestFullSyncSweepsSchemaListingsWithoutTotalResults(t *testing.T) {
	for _, resource := range []string{"schema-item-fields", "schema-creator-fields"} {
		t.Run(resource, func(t *testing.T) {
			syncTestWithHumanFriendly(t, true)
			db := syncTestOpenStore(t)
			defer db.Close()

			var script atomic.Pointer[func(string) totalsReply]
			script.Store(totalsConstant(totalsReply{body: `[{"field":"title","localized":"Title"}]`}))
			srv := totalsServer(t, &script)

			if _, err := db.UpsertKeyed(resource, []string{"retired"}, []json.RawMessage{json.RawMessage(`{"field":"retired","localized":"Retired"}`)}); err != nil {
				t.Fatalf("seed mirror: %v", err)
			}
			stderr := captureStderr(t, func() {
				if res := syncResource(context.Background(), syncTestClient(srv.URL), db, resource, 0, true, 0, false); res.Err != nil {
					t.Fatalf("full sync: %v", res.Err)
				}
			})
			ids, err := db.ResourceIDs(resource)
			if err != nil || ids["retired"] || !ids["title"] {
				t.Errorf("%s rows = %v (err %v), want the unlisted field reaped and title kept", resource, ids, err)
			}
			if strings.Contains(stderr, "Total-Results") {
				t.Errorf("stderr = %q, want no Total-Results warning for a schema listing", stderr)
			}
		})
	}
}

// The same count evidence guards the version checkpoint of an incremental
// pass: a short page the server contradicts, or a total that moves between
// pages, must not advance the cursor past changes it never delivered.
func TestIncrementalSyncRejectsContradictedPages(t *testing.T) {
	syncTestWithHumanFriendly(t, false)

	page := func(prefix string, n int) string {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("%s%03d", prefix, i)
		}
		return totalsItems(keys...)
	}
	for _, tc := range []struct {
		name   string
		script func(start string) totalsReply
	}{
		{name: "short page", script: func(string) totalsReply {
			return totalsReply{body: totalsItems("AAAA"), total: "3", version: "20"}
		}},
		{name: "total moves between pages", script: func(start string) totalsReply {
			if start == "0" || start == "" {
				return totalsReply{body: page("P", 100), total: "101", version: "20"}
			}
			return totalsReply{body: totalsItems("LAST"), total: "102", version: "20"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := syncTestOpenStore(t)
			defer db.Close()
			var script atomic.Pointer[func(string) totalsReply]
			script.Store(&tc.script)
			srv := totalsServer(t, &script)
			c := syncTestClient(srv.URL)
			if err := db.SaveLibraryVersion("items", c.Plane(), 10); err != nil {
				t.Fatalf("seed checkpoint: %v", err)
			}

			res := syncResource(context.Background(), c, db, "items", 0, false, 0, false)
			if res.Err == nil || !strings.Contains(res.Err.Error(), "Total-Results") {
				t.Fatalf("incremental sync err = %v, want a Total-Results refusal", res.Err)
			}
			if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "items"); v != 10 {
				t.Errorf("library version = %d, want 10 (unchanged)", v)
			}
		})
	}
}
