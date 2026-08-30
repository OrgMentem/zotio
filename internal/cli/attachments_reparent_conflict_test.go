// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reparentConflictFake is a focused stand-in for the write plane, built for the
// 412 retry path only. It answers GET /items/<key> with a parent and version it
// can change between calls, and it can fail a chosen number of PATCHes with 412
// before accepting one.
//
// It exists separately from reparentFake because these tests are about the
// retry's decision table, not the whole route, and a fake that only speaks the
// two verbs involved makes the branch under test obvious.
type reparentConflictFake struct {
	parent  string
	version int

	// conflictsLeft PATCHes are rejected with 412 before one is accepted.
	conflictsLeft int
	// onConflict runs after each rejected PATCH, so a test can move the object
	// the way the desktop's file registration does.
	onConflict func(f *reparentConflictFake)

	// targetWinnerMD5, when set, makes the TARGET already hold an attachment
	// with that hash, which is how a competing run finishing inside the retry
	// window looks to this route.
	targetWinnerMD5 string
	// attachTrashed makes the attachment read back as trashed, which is how
	// another actor's cleanup looks.
	attachTrashed int

	patchVersions []string
	gets          int
}

func (f *reparentConflictFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// The retry re-checks the target for a winner before each replay, and a
		// re-check it cannot complete fails closed. Serving an empty child list
		// is what "the target is still winner-free" looks like; targetWinnerMD5
		// lets a test make another run win mid-window instead.
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			w.Header().Set("Last-Modified-Version", fmt.Sprint(f.version))
			rows := []map[string]any{}
			if f.targetWinnerMD5 != "" {
				rows = append(rows, map[string]any{
					"key":     "WINNER01",
					"version": f.version,
					"data": map[string]any{
						"key": "WINNER01", "itemType": "attachment", "md5": f.targetWinnerMD5,
					},
				})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/0/items/"):
			f.gets++
			w.Header().Set("Last-Modified-Version", fmt.Sprint(f.version))
			body := map[string]any{"version": f.version, "data": map[string]any{
				"itemType": "attachment", "parentItem": f.parent, "deleted": f.attachTrashed,
			}}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPatch:
			f.patchVersions = append(f.patchVersions, r.Header.Get("If-Unmodified-Since-Version"))
			if f.conflictsLeft > 0 {
				f.conflictsLeft--
				if f.onConflict != nil {
					f.onConflict(f)
				}
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte("Item has been modified since specified version"))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestReparentRetriesTheDesktopsOwnVersionBump is the regression test for the
// defect this route hit on a real library: the connector creates the
// attachment, the DESKTOP then registers the stored file by writing md5 and
// mtime, and that bump invalidates the precondition the route resolved moments
// earlier. Measured live as "expected 15136, found 15137"; a PATCH replayed
// against the fresh version returned 204.
func TestReparentRetriesTheDesktopsOwnVersionBump(t *testing.T) {
	fake := &reparentConflictFake{parent: "TEMP0001", version: 15136, conflictsLeft: 1}
	// The desktop's registration lands between the route's read and its PATCH.
	fake.onConflict = func(f *reparentConflictFake) { f.version = 15137 }
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 15136); err != nil {
		t.Fatalf("reparentAttachment: %v, want success after one retry", err)
	}
	if got, want := fake.patchVersions, []string{"15136", "15137"}; !equalStrings(got, want) {
		t.Fatalf("PATCH preconditions = %v, want %v (the retry must carry the FRESH version)", got, want)
	}
}

// TestReparentTreatsAlreadyMovedAsSuccess covers the lost-response case: the
// PATCH landed, its reply did not arrive, and the retry's read finds the
// attachment already on the target. Reporting a conflict there would send the
// operator hunting for a problem that has already resolved itself.
func TestReparentTreatsAlreadyMovedAsSuccess(t *testing.T) {
	fake := &reparentConflictFake{parent: "TEMP0001", version: 900, conflictsLeft: 5}
	fake.onConflict = func(f *reparentConflictFake) { f.parent = "TARGET01" }
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 900); err != nil {
		t.Fatalf("reparentAttachment: %v, want success when the item is already on the target", err)
	}
	if len(fake.patchVersions) != 1 {
		t.Fatalf("PATCHes = %d, want 1: an already-moved item must not be patched again", len(fake.patchVersions))
	}
}

// TestReparentAbandonsWhenSomethingElseOwnsTheAttachment is the safety boundary.
// The retry is only defensible while the attachment is still a child of the
// temporary parent this run created. If it has moved somewhere else, another
// actor owns it, and forcing the move would be exactly the clobber the
// precondition exists to prevent.
func TestReparentAbandonsWhenSomethingElseOwnsTheAttachment(t *testing.T) {
	fake := &reparentConflictFake{parent: "TEMP0001", version: 900, conflictsLeft: 5}
	fake.onConflict = func(f *reparentConflictFake) {
		f.parent = "SOMEONEELSE"
		f.version = 999
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 900)
	if err == nil {
		t.Fatal("reparentAttachment succeeded, want the original conflict returned")
	}
	if len(fake.patchVersions) != 1 {
		t.Fatalf("PATCHes = %d, want 1: a foreign parent must stop the retry", len(fake.patchVersions))
	}
}

// TestReparentDoesNotSpinOnAStaticVersion pins that a 412 whose version has not
// moved is not retried. Replaying an identical precondition would produce an
// identical rejection, so the loop must exit rather than burn the budget.
func TestReparentDoesNotSpinOnAStaticVersion(t *testing.T) {
	fake := &reparentConflictFake{parent: "TEMP0001", version: 900, conflictsLeft: 5}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 900); err == nil {
		t.Fatal("reparentAttachment succeeded, want the conflict returned")
	}
	if len(fake.patchVersions) != 1 {
		t.Fatalf("PATCHes = %d, want 1: an unmoved version must not be retried", len(fake.patchVersions))
	}
}

// TestReparentRetryIsBounded pins the budget. A version that keeps advancing
// must not let the route retry forever against a library that is actively
// being written by something else.
func TestReparentRetryIsBounded(t *testing.T) {
	fake := &reparentConflictFake{parent: "TEMP0001", version: 900, conflictsLeft: 99}
	fake.onConflict = func(f *reparentConflictFake) { f.version++ }
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 900); err == nil {
		t.Fatal("reparentAttachment succeeded, want the conflict returned")
	}
	if got, want := len(fake.patchVersions), connectorReparentConflictRetries+1; got != want {
		t.Fatalf("PATCHes = %d, want %d (retries capped by connectorReparentConflictRetries)", got, want)
	}
}

// TestReparentAbandonsWhenTheTargetGainsTheContentMidRetry pins the fix for a
// hazard found in review of the retry itself. The reconciliation that checks the
// target for a winner runs ONCE, before the first PATCH. A competing run can
// finish inside the retry window, and the ownership check does not notice: it
// proves the attachment is still ours, not that the target is still winner-free.
// Replaying blindly would put a second identical attachment on the operator's
// item.
//
// Without the re-check this test sees a successful second PATCH instead of the
// race-lost signal, which is exactly the duplicate.
func TestReparentAbandonsWhenTheTargetGainsTheContentMidRetry(t *testing.T) {
	fake := &reparentConflictFake{parent: "TEMP0001", version: 15136, conflictsLeft: 1}
	fake.onConflict = func(f *reparentConflictFake) {
		f.version++
		// Another run completed while we were being rejected.
		f.targetWinnerMD5 = "deadbeef"
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 15136)
	var raceLost *reparentRaceLostError
	if !errors.As(err, &raceLost) {
		t.Fatalf("err = %v, want a reparentRaceLostError so the caller cleans up instead of duplicating", err)
	}
	if raceLost.winner != "WINNER01" {
		t.Errorf("winner = %q, want WINNER01 carried with the error rather than looked up again", raceLost.winner)
	}
	if len(fake.patchVersions) != 1 {
		t.Errorf("PATCHes = %d, want 1: the replay must not happen once the target has the content", len(fake.patchVersions))
	}
}

// TestReparentDoesNotResurrectATrashedAttachment pins that another actor's
// cleanup is respected. If our attachment was deliberately trashed between the
// PATCH and the re-read, replaying the move would resurrect it onto the target,
// undoing a decision some other run made on purpose.
func TestReparentDoesNotResurrectATrashedAttachment(t *testing.T) {
	fake := &reparentConflictFake{parent: "TEMP0001", version: 900, conflictsLeft: 1}
	fake.onConflict = func(f *reparentConflictFake) {
		f.version++
		f.attachTrashed = 1
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 900); err == nil {
		t.Fatal("reparentAttachment succeeded, want the conflict returned for a trashed attachment")
	}
	if len(fake.patchVersions) != 1 {
		t.Errorf("PATCHes = %d, want 1: a trashed attachment must not be moved", len(fake.patchVersions))
	}
}

// TestReparentFailsClosedWhenTheTargetCannotBeRechecked pins that a re-check it
// cannot complete is not permission to replay. Without a completed read there is
// no proof the target is winner-free, and guessing wrong duplicates content onto
// the operator's item.
func TestReparentFailsClosedWhenTheTargetCannotBeRechecked(t *testing.T) {
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/children"):
			// A transport-level failure, not a 404: no answer either way.
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPatch:
			patches++
			w.Header().Set("Last-Modified-Version", "901")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte("Item has been modified since specified version"))
		default:
			w.Header().Set("Last-Modified-Version", "901")
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 901,
				"data": map[string]any{"itemType": "attachment", "parentItem": "TEMP0001"}})
		}
	}))
	t.Cleanup(srv.Close)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 900); err == nil {
		t.Fatal("reparentAttachment succeeded despite being unable to re-check the target")
	}
	if patches != 1 {
		t.Errorf("PATCHes = %d, want 1: an unverifiable target must not be replayed onto", patches)
	}
}

// TestReparentDoesNotRetryANonConflict pins that only 412 is retried. A 403 or
// a 404 is a different problem, and re-reading then replaying would hide it.
func TestReparentDoesNotRetryANonConflict(t *testing.T) {
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Last-Modified-Version", "901")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 901,
			"data": map[string]any{"itemType": "attachment", "parentItem": "TEMP0001"}})
	}))
	t.Cleanup(srv.Close)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 900); err == nil {
		t.Fatal("reparentAttachment succeeded on a 403, want the error")
	}
	if patches != 1 {
		t.Fatalf("PATCHes = %d, want 1: a non-412 must not be retried", patches)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
