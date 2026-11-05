// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// TestTailWebhookFailureHoldsCursorAndReplaysBatch pins the cursor invariant
// behind finding zotio-e367b8eb8135612e: a batch the webhook sink rejects must
// not be skipped. The first poll errors and leaves the cursor where it was;
// the next poll asks for the same window again and delivers the same events.
func TestTailWebhookFailureHoldsCursorAndReplaysBatch(t *testing.T) {
	var sinces []string
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		sinces = append(sinces, r.URL.Query().Get("since"))
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `[{"key":"Z","version":7}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		// Unsupported plane, like the live local API: deletions must not be
		// the reason the cursor is held, so the webhook failure is isolated
		// as the only hold.
		http.Error(w, "No endpoint found", http.StatusNotFound)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	var bodies [][]byte
	failWebhook := true
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if failWebhook {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		bodies = append(bodies, b)
	}))
	defer hook.Close()

	c := client.New(&config.Config{BaseURL: api.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", api.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })

	sink := DeliverSink{Scheme: "webhook", Target: hook.URL}

	var first bytes.Buffer
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", sink, &first, nil); err == nil {
		t.Fatal("emitChanges succeeded despite webhook delivery failure")
	} else if !strings.Contains(err.Error(), "delivering webhook") {
		t.Fatalf("error = %q, want it to name the webhook delivery failure", err.Error())
	}
	if got, _, err := db.StoredLibraryVersionContext(context.Background(), "tail:items"); err != nil || got != 6 {
		t.Fatalf("tail cursor = %d, %v; want unchanged 6", got, err)
	}
	if len(bodies) != 0 {
		t.Fatalf("webhook received %d bodies on the failed poll, want 0 accepted", len(bodies))
	}
	firstEvents := ndjsonEvents(t, first.String())
	if len(firstEvents) != 1 || firstEvents[0]["key"] != "Z" {
		t.Fatalf("failed poll stdout = %q, want the undelivered upsert Z", first.String())
	}

	failWebhook = false
	var second bytes.Buffer
	n, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", sink, &second, nil)
	if err != nil {
		t.Fatalf("replay emitChanges: %v", err)
	}
	if n != 1 {
		t.Fatalf("replay emitted = %d, want 1", n)
	}
	if len(sinces) != 2 || sinces[0] != "6" || sinces[1] != "6" {
		t.Fatalf("poll since values = %q, want [6 6]: the failed window was not retried", sinces)
	}
	if len(bodies) != 1 {
		t.Fatalf("webhook accepted %d bodies, want 1", len(bodies))
	}
	replayed := ndjsonEvents(t, string(bodies[0]))
	if len(replayed) != 1 || replayed[0]["event"] != "upsert" || replayed[0]["key"] != "Z" {
		t.Fatalf("replayed webhook body = %q, want the upsert Z offered again", bodies[0])
	}
	if got, _, err := db.StoredLibraryVersionContext(context.Background(), "tail:items"); err != nil || got != 7 {
		t.Fatalf("tail cursor = %d, %v; want 7 after the batch was delivered", got, err)
	}
}

// TestTailFileFailureHoldsCursorAndReplaysBatch is the file-sink half of the
// same invariant: a batch the file sink cannot take must be retried, not
// skipped. The first poll targets a path blocked by a regular file, so
// creating the delivery directory fails; after the block is removed the same
// events arrive in the delivery file and the cursor advances.
func TestTailFileFailureHoldsCursorAndReplaysBatch(t *testing.T) {
	var sinces []string
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		sinces = append(sinces, r.URL.Query().Get("since"))
		w.Header().Set("Last-Modified-Version", "9")
		_, _ = io.WriteString(w, `[{"key":"F","version":9}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "No endpoint found", http.StatusNotFound)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	c := client.New(&config.Config{BaseURL: api.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", api.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("planting blocker file: %v", err)
	}
	target := filepath.Join(blocker, "events.ndjson")
	sink := DeliverSink{Scheme: "file", Target: target}

	var first bytes.Buffer
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", sink, &first, nil); err == nil {
		t.Fatal("emitChanges succeeded despite the blocked delivery file")
	}
	if got, _, err := db.StoredLibraryVersionContext(context.Background(), "tail:items"); err != nil || got != 6 {
		t.Fatalf("tail cursor = %d, %v; want unchanged 6", got, err)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatalf("removing blocker file: %v", err)
	}
	var second bytes.Buffer
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", sink, &second, nil); err != nil {
		t.Fatalf("replay emitChanges: %v", err)
	}
	if len(sinces) != 2 || sinces[0] != "6" || sinces[1] != "6" {
		t.Fatalf("poll since values = %q, want [6 6]: the failed window was not retried", sinces)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading delivery file: %v", err)
	}
	fileEvents := ndjsonEvents(t, string(data))
	if len(fileEvents) != 1 || fileEvents[0]["event"] != "upsert" || fileEvents[0]["key"] != "F" {
		t.Fatalf("delivery file = %q, want the upsert F offered again", data)
	}
	if got, _, err := db.StoredLibraryVersionContext(context.Background(), "tail:items"); err != nil || got != 9 {
		t.Fatalf("tail cursor = %d, %v; want 9 after the batch was delivered", got, err)
	}
}

// TestEmitChangesWithHook_RecordsOwedTriggerBeforeItRuns keeps the crash
// window of finding zotio-e0407c709ebe1b15 closed under the fix for
// zotio-1199a7b2032d6489: the owed-trigger record is durable before the
// trigger runs and before the cursor moves past the delivered events, so a
// kill during the workflow still owes it. A failed trigger keeps the record;
// the retry runs only the workflow and re-delivers nothing.
func TestEmitChangesWithHook_RecordsOwedTriggerBeforeItRuns(t *testing.T) {
	var sinces []string
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		sinces = append(sinces, since)
		w.Header().Set("Last-Modified-Version", "7")
		if since == "7" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, `[{"key":"A","version":7}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `{"items":[],"collections":[],"searches":[],"tags":[]}`)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	c := client.New(&config.Config{BaseURL: api.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", api.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var hookRuns int
	var cursorAtHook int
	var recordedAtHook tailChangeBatch
	var hook *tailTriggerHook
	hook = tailTestHook(t, func(batch tailChangeBatch) error {
		hookRuns++
		recorded, err := loadTailPendingTrigger(hook.pendingPath("items", api.URL))
		if err != nil {
			return err
		}
		recordedAtHook = recorded
		v, _, err := db.StoredLibraryVersionContext(context.Background(), "tail:items")
		if err != nil {
			return err
		}
		cursorAtHook = v
		if hookRuns == 1 {
			return fmt.Errorf("workflow failed")
		}
		return nil
	})

	var out bytes.Buffer
	_, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, hook)
	if err == nil || !strings.Contains(err.Error(), "workflow failed") {
		t.Fatalf("first trigger error = %v, want workflow failure", err)
	}
	if cursorAtHook != 7 {
		t.Errorf("cursor during trigger = %d, want 7: delivered events are recorded as owed, not replayed", cursorAtHook)
	}
	if len(recordedAtHook.Events) != 1 || recordedAtHook.Events[0].Key != "A" {
		t.Errorf("owed record during trigger = %+v, want upsert A recorded before the run", recordedAtHook)
	}
	if events := ndjsonEvents(t, out.String()); len(events) != 1 || events[0]["key"] != "A" {
		t.Fatalf("stdout = %q, want the upsert A delivered before the trigger", out.String())
	}

	out.Reset()
	n, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, hook)
	if err != nil {
		t.Fatalf("emitChangesWithHook: %v", err)
	}
	if n != 0 || out.Len() != 0 {
		t.Fatalf("retry emitted %d event(s) %q, want only the workflow retried", n, out.String())
	}
	if hookRuns != 2 {
		t.Fatalf("hook ran %d times, want failed attempt and retry", hookRuns)
	}
	if len(sinces) != 2 || sinces[0] != "6" || sinces[1] != "7" {
		t.Fatalf("since values = %v, want [6 7]: the retry must not re-read the delivered window", sinces)
	}
	if _, err := os.Stat(hook.pendingPath("items", api.URL)); !os.IsNotExist(err) {
		t.Fatalf("owed record after the successful retry: %v, want it removed", err)
	}
}
