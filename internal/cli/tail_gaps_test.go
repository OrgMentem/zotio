// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/store"
)

func tailTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func ndjsonEvents(t *testing.T, s string) []map[string]any {
	t.Helper()
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(s, "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decoding NDJSON line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// TestEmitChanges_ChangeFeed exercises the full bootstrap-then-delta cycle:
// the first poll emits the current set as upserts and records the cursor; the
// second poll fetches only since the cursor and surfaces /deleted keys.
func TestEmitChanges_ChangeFeed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		switch since := r.URL.Query().Get("since"); since {
		case "":
			w.Header().Set("Last-Modified-Version", "10")
			_, _ = io.WriteString(w, `[{"key":"A","version":5,"data":{}},{"key":"B","version":5}]`)
		case "10":
			w.Header().Set("Last-Modified-Version", "12")
			_, _ = io.WriteString(w, `[{"key":"A","version":12}]`)
		default:
			t.Errorf("/items unexpected since=%q", since)
		}
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		if since := r.URL.Query().Get("since"); since != "10" {
			t.Errorf("/deleted unexpected since=%q", since)
		}
		w.Header().Set("Last-Modified-Version", "12")
		_, _ = io.WriteString(w, `{"items":["B"],"collections":[],"searches":[],"tags":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)

	// Baseline poll: no cursor yet -> full set as upserts, no deletions.
	var buf bytes.Buffer
	if _, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf); err != nil {
		t.Fatalf("baseline emitChanges: %v", err)
	}
	events := ndjsonEvents(t, buf.String())
	if len(events) != 2 {
		t.Fatalf("baseline: want 2 events, got %d: %q", len(events), buf.String())
	}
	gotKeys := map[string]bool{}
	for _, ev := range events {
		if ev["event"] != "upsert" {
			t.Errorf("baseline event = %v, want upsert", ev["event"])
		}
		gotKeys[ev["key"].(string)] = true
	}
	if !gotKeys["A"] || !gotKeys["B"] {
		t.Errorf("baseline keys = %v, want A and B", gotKeys)
	}
	if v, _ := db.GetLibraryVersion("tail:items"); v != 10 {
		t.Errorf("cursor after baseline = %d, want 10", v)
	}

	// Delta poll: since=10 -> one upsert (A) plus one delete (B).
	buf.Reset()
	if _, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf); err != nil {
		t.Fatalf("delta emitChanges: %v", err)
	}
	events = ndjsonEvents(t, buf.String())
	if len(events) != 2 {
		t.Fatalf("delta: want 2 events, got %d: %q", len(events), buf.String())
	}
	var upserts, deletes int
	for _, ev := range events {
		switch ev["event"] {
		case "upsert":
			upserts++
			if ev["key"] != "A" {
				t.Errorf("delta upsert key = %v, want A", ev["key"])
			}
		case "delete":
			deletes++
			if ev["key"] != "B" {
				t.Errorf("delta delete key = %v, want B", ev["key"])
			}
		default:
			t.Errorf("delta unexpected event %v", ev["event"])
		}
	}
	if upserts != 1 || deletes != 1 {
		t.Errorf("delta: upserts=%d deletes=%d, want 1 and 1", upserts, deletes)
	}
	if v, _ := db.GetLibraryVersion("tail:items"); v != 12 {
		t.Errorf("cursor after delta = %d, want 12", v)
	}
}

// TestEmitChanges_WebhookDelivery verifies that each cycle's NDJSON is POSTed
// to a webhook sink in addition to being written to the local writer.
func TestEmitChanges_WebhookDelivery(t *testing.T) {
	var received [][]byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = append(received, b)
	}))
	defer hook.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `[{"key":"Z","version":1}]`)
	}))
	defer api.Close()

	c := client.New(&config.Config{BaseURL: api.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)

	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	var buf bytes.Buffer
	if _, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "webhook", Target: hook.URL}, &buf); err != nil {
		t.Fatalf("emitChanges: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("webhook received %d bodies, want 1", len(received))
	}
	if !strings.Contains(string(received[0]), `"event":"upsert"`) {
		t.Errorf("webhook body missing upsert event: %s", received[0])
	}
	if !strings.Contains(buf.String(), `"key":"Z"`) {
		t.Errorf("stdout missing event: %s", buf.String())
	}
}

func TestEmitChangesFileSinkCreatesNestedParentDirs(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "9")
		_, _ = io.WriteString(w, `[{"key":"NESTED","version":9}]`)
	}))
	defer api.Close()

	c := client.New(&config.Config{BaseURL: api.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	target := filepath.Join(t.TempDir(), "nested", "private", "events.ndjson")

	var buf bytes.Buffer
	if _, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "file", Target: target}, &buf); err != nil {
		t.Fatalf("emitChanges: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading file sink: %v", err)
	}
	if !strings.Contains(string(data), `"key":"NESTED"`) {
		t.Fatalf("file sink data missing event: %s", data)
	}
	info, err := os.Stat(filepath.Dir(target))
	if err != nil {
		t.Fatalf("stat file sink parent: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("file sink parent mode = %o, want 700", got)
	}
}

func TestTailFollowReturnsOnContextCancelAndStopsPolling(t *testing.T) {
	var requests atomic.Int64
	firstPollDone := make(chan struct{})
	var firstPollClosed atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Last-Modified-Version", "1")
		_, _ = io.WriteString(w, `[]`)
		if firstPollClosed.CompareAndSwap(false, true) {
			close(firstPollDone)
		}
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Last-Modified-Version", "1")
		_, _ = io.WriteString(w, `{"items":[],"collections":[],"searches":[],"tags":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := RootCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{
		"tail",
		"items",
		"--follow=true",
		"--interval=20ms",
		"--db", filepath.Join(t.TempDir(), "tail.db"),
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Execute()
	}()

	select {
	case <-firstPollDone:
	case err := <-errCh:
		t.Fatalf("tail returned before first poll: %v", err)
	case <-time.After(time.Second):
		t.Fatal("tail did not perform initial poll")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("tail error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tail did not return promptly after context cancellation")
	}

	afterCancel := requests.Load()
	time.Sleep(3 * 20 * time.Millisecond)
	if got := requests.Load(); got != afterCancel {
		t.Fatalf("requests after cancellation = %d, want unchanged from %d", got, afterCancel)
	}
}
