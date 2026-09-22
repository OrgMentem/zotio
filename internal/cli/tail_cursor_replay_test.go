// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
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
	if _, err := emitChanges(context.Background(), c, db, "items", "/items", sink, &first); err == nil {
		t.Fatal("emitChanges succeeded despite webhook delivery failure")
	} else if !strings.Contains(err.Error(), "delivering webhook") {
		t.Fatalf("error = %q, want it to name the webhook delivery failure", err.Error())
	}
	if got, _, err := db.StoredLibraryVersion("tail:items"); err != nil || got != 6 {
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
	n, err := emitChanges(context.Background(), c, db, "items", "/items", sink, &second)
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
	if got, _, err := db.StoredLibraryVersion("tail:items"); err != nil || got != 7 {
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
	if _, err := emitChanges(context.Background(), c, db, "items", "/items", sink, &first); err == nil {
		t.Fatal("emitChanges succeeded despite the blocked delivery file")
	}
	if got, _, err := db.StoredLibraryVersion("tail:items"); err != nil || got != 6 {
		t.Fatalf("tail cursor = %d, %v; want unchanged 6", got, err)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatalf("removing blocker file: %v", err)
	}
	var second bytes.Buffer
	if _, err := emitChanges(context.Background(), c, db, "items", "/items", sink, &second); err != nil {
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
	if got, _, err := db.StoredLibraryVersion("tail:items"); err != nil || got != 9 {
		t.Fatalf("tail cursor = %d, %v; want 9 after the batch was delivered", got, err)
	}
}
