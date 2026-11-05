// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

func tailTestClient(url string) *client.Client {
	c := client.New(&config.Config{BaseURL: url}, 5*time.Second, 0)
	c.NoCache = true
	return c
}

// A first poll records its baseline listing as owed and a crash stops tail
// before the record is cleared. A real change delivered by the next poll must
// reach a workflow that reads trigger values; merged into the baseline it was
// skipped as "the first poll" and cleared for good.
func TestTailOwedBaselineDoesNotSwallowALaterChange(t *testing.T) {
	lib := &tailGrowingLibrary{version: 5}
	lib.add("AAAA0001")
	api := lib.server(t)
	c := tailTestClient(api.URL)
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", api.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}
	var got []tailChangeBatch
	hook := tailTestHook(t, func(batch tailChangeBatch) error {
		got = append(got, batch)
		return nil
	})
	baseline := tailChangeBatch{Baseline: true, Events: []workflowRunTriggerEvent{{Event: "upsert", Resource: "items", Key: "AAAA0001"}}}
	if err := saveTailPendingTrigger(hook.pendingPath("items", api.URL), baseline); err != nil {
		t.Fatal(err)
	}
	lib.add("BBBB0002")

	var out bytes.Buffer
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, hook); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("trigger ran %d times, want 1", len(got))
	}
	if got[0].Baseline || !reflect.DeepEqual(batchKeys(got[0]), []string{"BBBB0002"}) {
		t.Fatalf("trigger batch = %+v, want the change BBBB0002 alone, not a baseline", got[0])
	}
	trigger, skip := tailWorkflowTrigger("items", got[0])
	if skip != "" || !reflect.DeepEqual(trigger.UpsertKeys, []string{"BBBB0002"}) {
		t.Fatalf("trigger = %+v, skip %q; want upsert BBBB0002", trigger, skip)
	}
}

// The other direction: real changes are owed and the cursor resets, so the
// next poll is a baseline listing. The owed changes must still reach the
// workflow as changes.
func TestTailNewBaselineDoesNotSwallowOwedChanges(t *testing.T) {
	lib := &tailGrowingLibrary{version: 5}
	lib.add("AAAA0001")
	lib.add("BBBB0002")
	api := lib.server(t)
	c := tailTestClient(api.URL)
	db := tailTestStore(t)
	var got []tailChangeBatch
	hook := tailTestHook(t, func(batch tailChangeBatch) error {
		got = append(got, batch)
		return nil
	})
	owed := tailChangeBatch{Events: []workflowRunTriggerEvent{{Event: "upsert", Resource: "items", Key: "AAAA0001"}}}
	if err := saveTailPendingTrigger(hook.pendingPath("items", api.URL), owed); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, hook); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(got) != 1 || got[0].Baseline || !reflect.DeepEqual(batchKeys(got[0]), []string{"AAAA0001"}) {
		t.Fatalf("trigger batches = %+v, want the owed change AAAA0001 alone, not a baseline", got)
	}
}

// Two API sources share one --db and one workflow spec. A workflow owed on
// source A must not receive A's keys while tail reads source B.
func TestTailOwedTriggerIsKeptPerAPISource(t *testing.T) {
	libA := &tailGrowingLibrary{version: 6}
	apiA := libA.server(t)
	libB := &tailGrowingLibrary{version: 6}
	apiB := libB.server(t)
	db := tailTestStore(t)

	fail := true
	var got [][]string
	hook := tailTestHook(t, func(batch tailChangeBatch) error {
		got = append(got, batchKeys(batch))
		if fail {
			return errors.New("workflow failed")
		}
		return nil
	})
	var out bytes.Buffer

	if err := db.SaveLibraryVersion("tail:items", apiA.URL, 6); err != nil {
		t.Fatal(err)
	}
	libA.add("AAAA0001")
	if _, err := emitChangesWithHook(context.Background(), tailTestClient(apiA.URL), db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, hook); err == nil {
		t.Fatal("source A: want the workflow failure")
	}

	fail = false
	if err := db.SaveLibraryVersion("tail:items", apiB.URL, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := emitChangesWithHook(context.Background(), tailTestClient(apiB.URL), db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, hook); err != nil {
		t.Fatalf("source B: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("trigger batches = %v; source B owes nothing, so its poll must not run source A's workflow", got)
	}
	if _, err := os.Stat(hook.pendingPath("items", apiA.URL)); err != nil {
		t.Fatalf("source A's owed record: %v, want it kept for source A", err)
	}
}

// Two spellings of one database (a symlink) must find one owed record.
func TestTailOwedTriggerIsFoundThroughADatabaseSymlink(t *testing.T) {
	lib := &tailGrowingLibrary{version: 6}
	lib.add("AAAA0001")
	api := lib.server(t)
	c := tailTestClient(api.URL)
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", api.URL, 6); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	realDB := filepath.Join(dir, "mirror.db")
	if err := os.WriteFile(realDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	linkDB := filepath.Join(dir, "mirror-link.db")
	if err := os.Symlink(realDB, linkDB); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	spec := filepath.Join(dir, "spec.json")

	var out bytes.Buffer
	failing := tailTestHookAt(t, realDB, spec, func(tailChangeBatch) error { return errors.New("workflow failed") })
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, failing); err == nil {
		t.Fatal("want the workflow failure")
	}
	var got []string
	viaLink := tailTestHookAt(t, linkDB, spec, func(batch tailChangeBatch) error {
		got = batchKeys(batch)
		return nil
	})
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, viaLink); err != nil {
		t.Fatalf("emit via the symlink: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"AAAA0001"}) {
		t.Fatalf("trigger via the symlinked --db got %v, want the owed AAAA0001", got)
	}
}

// A --db symlink whose target the first poll has not created yet must name
// the same owed-trigger record as the link after the database exists, and as
// the real path; otherwise a restart cannot find the debt.
func TestTailOwedTriggerRecordIsStableThroughADanglingDatabaseSymlink(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	realDB := filepath.Join(realDir, "tail.db")
	linkDB := filepath.Join(dir, "tail-link.db")
	if err := os.Symlink(realDB, linkDB); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	spec := filepath.Join(dir, "spec.json")
	noop := func(tailChangeBatch) error { return nil }

	before := tailTestHookAt(t, linkDB, spec, noop).pendingPath("items", "http://source")
	if err := os.WriteFile(realDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	after := tailTestHookAt(t, linkDB, spec, noop).pendingPath("items", "http://source")
	direct := tailTestHookAt(t, realDB, spec, noop).pendingPath("items", "http://source")
	if before != after || after != direct {
		t.Fatalf("record path before the database exists %q, after %q, via the real path %q; want one path", before, after, direct)
	}
}

// A tag is deleted, the workflow fails, and the tag is created again. The
// owed batch must report the tag as upserted: deletions win inside one
// trigger, so keeping the stale delete reported the live tag as deleted.
func TestTailOwedDeleteIsReplacedByALaterUpsert(t *testing.T) {
	poll := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := 7 + poll
		w.Header().Set("Last-Modified-Version", fmt.Sprint(version))
		switch {
		case r.URL.Path == "/deleted" && poll == 0:
			_, _ = fmt.Fprint(w, `{"tags":["research"]}`)
		case r.URL.Path == "/deleted":
			_, _ = fmt.Fprint(w, `{"tags":[]}`)
		case poll == 0:
			_, _ = fmt.Fprint(w, `[]`)
		default:
			_, _ = fmt.Fprintf(w, `[{"tag":"research","version":%d}]`, version)
		}
	}))
	defer srv.Close()
	c := tailTestClient(srv.URL)
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:tags", srv.URL, 6); err != nil {
		t.Fatal(err)
	}
	var got []tailChangeBatch
	hook := tailTestHook(t, func(batch tailChangeBatch) error {
		got = append(got, batch)
		if len(got) == 1 {
			return errors.New("workflow failed")
		}
		return nil
	})
	var out bytes.Buffer
	if _, err := emitChangesWithHook(context.Background(), c, db, "tags", "/tags", DeliverSink{Scheme: "stdout"}, &out, hook); err == nil {
		t.Fatal("want the workflow failure")
	}
	poll++
	if _, err := emitChangesWithHook(context.Background(), c, db, "tags", "/tags", DeliverSink{Scheme: "stdout"}, &out, hook); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	want := []workflowRunTriggerEvent{{Event: "upsert", Resource: "tags", Key: "research"}}
	if len(got) != 2 || !reflect.DeepEqual(got[1].Events, want) {
		t.Fatalf("trigger batches = %+v, want the recreated tag as one upsert", got)
	}
	trigger, skip := tailWorkflowTrigger("tags", got[1])
	if skip != "" || !reflect.DeepEqual(trigger.UpsertKeys, []string{"research"}) || len(trigger.DeleteKeys) != 0 {
		t.Fatalf("trigger = %+v, skip %q; want research upserted and nothing deleted", trigger, skip)
	}
}

// When unread deletions hold the cursor, the window's upserts are delivered
// again, so the held-trigger message must not promise otherwise.
func TestTailHeldTriggerMessageNamesAHeldCursor(t *testing.T) {
	held := (&tailTriggerHeldError{resource: "items", delivered: 2, owed: 3, cursorHeld: true, err: errors.New("workflow failed")}).Error()
	if strings.Contains(held, "not re-delivered") || strings.Contains(held, "retries only the workflow") {
		t.Fatalf("held-cursor message = %q, must not claim the upserts stay undelivered", held)
	}
	if !strings.Contains(held, "delivers this window's upserts again") {
		t.Fatalf("held-cursor message = %q, want it to say the upserts are delivered again", held)
	}
	advanced := (&tailTriggerHeldError{resource: "items", delivered: 2, owed: 3, err: errors.New("workflow failed")}).Error()
	if !strings.Contains(advanced, "not re-delivered") {
		t.Fatalf("advanced-cursor message = %q, want it to say nothing is re-delivered", advanced)
	}
}
