// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/store"
)

// malformed body must not advance cursor: the transient 2xx should be retried
// on the next poll rather than permanently skipped.
func TestEmitChanges_MalformedPageDoesNotAdvanceCursor(t *testing.T) {
	db := tailTestStore(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "10" {
			t.Errorf("since = %q, want 10", got)
		}
		w.Header().Set("Last-Modified-Version", "11")
		_, _ = io.WriteString(w, "not-json")
	})
	// /deleted must not be reached because emitChanges returns before it.
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		t.Error("/deleted should not be fetched when page is malformed")
		w.Header().Set("Last-Modified-Version", "11")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 10); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	_, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("emitChanges with malformed body: want error, got nil")
	}
	if !strings.Contains(err.Error(), "decoding change page") {
		t.Errorf("error = %q, want to mention decoding change page", err.Error())
	}
	if buf.Len() != 0 {
		t.Errorf("writer got %q, want empty on malformed page", buf.String())
	}
	// Cursor must remain at 10; advancing to 11 would skip the unseen changes.
	if v, src, _ := db.StoredLibraryVersion("tail:items"); v != 10 || src != srv.URL {
		t.Errorf("cursor after malformed page = (%d,%q), want (10,%q)", v, src, srv.URL)
	}
}

// Singleton envelope is valid JSON but never a tail page; treat as decoding
// failure and do not advance.
func TestEmitChanges_SingletonEnvelopeDoesNotAdvanceCursor(t *testing.T) {
	db := tailTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "5")
		_, _ = io.WriteString(w, `{"key":"ABC123","version":1,"data":{"title":"hi"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true

	var buf bytes.Buffer
	_, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("singleton envelope: want error, got nil")
	}
	if v, _, _ := db.StoredLibraryVersion("tail:items"); v != 0 {
		t.Errorf("cursor after singleton = %d, want 0 (unchanged)", v)
	}
}

// Empty page is the legitimate "no changes" signal and must advance the cursor
// (the invariant from the task: gate on err==nil && isPage, not on emitted>0).
func TestEmitChanges_EmptyPageAdvancesCursor(t *testing.T) {
	db := tailTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `{"items":[],"collections":[],"searches":[],"tags":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true

	var buf bytes.Buffer
	n, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if n != 0 {
		t.Errorf("empty page emitted %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Errorf("empty page wrote %q, want empty", buf.String())
	}
	if v, src, _ := db.StoredLibraryVersion("tail:items"); v != 7 || src != srv.URL {
		t.Errorf("cursor after empty page = (%d,%q), want (7,%q)", v, src, srv.URL)
	}
}

// Malformed /deleted payload must surface as error and not advance the cursor
// past unseen deletions.
func TestEmitChanges_MalformedDeletionsDoesNotAdvanceCursor(t *testing.T) {
	db := tailTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "12")
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "12")
		_, _ = io.WriteString(w, `not-json`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true

	// Seed after srv is known so the source matches.
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 10); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	_, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("malformed deletions: want error, got nil")
	}
	if !strings.Contains(err.Error(), "decoding deletions") {
		t.Errorf("error = %q, want to mention decoding deletions", err.Error())
	}
	if v, src, _ := db.StoredLibraryVersion("tail:items"); v != 10 || src != srv.URL {
		t.Errorf("cursor after malformed deletions = (%d,%q), want (10,%q)", v, src, srv.URL)
	}
}

// --follow=false with cmd.SetOut must capture NDJSON on the Cobra writer (not
// os.Stdout), exercising both call sites now routed through cmd.OutOrStdout().
func TestTail_FollowFalseWritesToCobraOut(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "1")
		_, _ = io.WriteString(w, `[{"key":"X","version":1,"data":{"title":"hello"}}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	cmd := RootCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"tail", "items", "--follow=false", "--db", filepath.Join(t.TempDir(), "tail.db")})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("tail --follow=false: %v (stderr %q)", err, errOut.String())
	}
	events := ndjsonEvents(t, out.String())
	if len(events) != 1 {
		t.Fatalf("want 1 event in Cobra Out, got %d: %q", len(events), out.String())
	}
	if events[0]["event"] != "upsert" || events[0]["key"] != "X" {
		t.Errorf("event = %v, want upsert/X", events[0])
	}
}

// A /deleted failure has two cases with OPPOSITE correct responses, so the
// cursor rule cannot be uniform. Both are pinned here.
//
// Case 1: the plane does not implement /deleted. The Zotero local API 404s it
// and local is the default base, so this is the steady state for most users.
// Advancing is mandatory: no deletion is observable on this plane ever, so
// none can be lost, while holding the cursor would wedge the feed permanently
// and re-emit the entire window on every poll.
func TestEmitChanges_UnsupportedDeletionsStillAdvancesCursor(t *testing.T) {
	db := tailTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "12")
		_, _ = io.WriteString(w, `[{"key":"A","version":12,"data":{"title":"t"}}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		// Verbatim shape of the live local API: 404 "No endpoint found".
		http.Error(w, "No endpoint found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 10); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	emitted, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err != nil {
		t.Fatalf("unsupported /deleted must not fail the poll: %v", err)
	}
	if emitted != 1 {
		t.Errorf("emitted = %d, want the upsert still delivered", emitted)
	}
	if v, _, _ := db.StoredLibraryVersion("tail:items"); v != 12 {
		t.Fatalf("cursor = %d, want 12: an unimplemented /deleted must not wedge the feed", v)
	}
}

// Case 2: the request failed for any other reason. Deletions may well exist in
// this window and simply were not retrieved, so advancing past them loses them
// permanently -- the next poll asks only for changes strictly after newVer.
// Hold the cursor and retry the window; the re-emitted upserts are the
// at-least-once duplication a change feed is allowed to have.
//
// The poll must still return nil: emitChanges errors terminate the whole
// --follow loop (see newTailCmd), so failing here would turn a recoverable
// blip into a dead tail.
func TestEmitChanges_DeletionsFetchFailureRetainsCursor(t *testing.T) {
	db := tailTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "12")
		_, _ = io.WriteString(w, `[{"key":"A","version":12,"data":{"title":"t"}}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 10); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	emitted, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err != nil {
		t.Fatalf("a recoverable deletions failure must not kill the tail: %v", err)
	}
	if emitted != 1 {
		t.Errorf("emitted = %d, want the upsert still delivered", emitted)
	}
	if v, src, _ := db.StoredLibraryVersion("tail:items"); v != 10 || src != srv.URL {
		t.Fatalf("cursor = (%d,%q), want (10,%q): unread deletions must be retried, not skipped", v, src, srv.URL)
	}
}

// A non-positive --interval used to reach time.NewTicker in follow mode and
// panic with a Go runtime message plus a stack trace. Bad flag input must be
// a usage error (exit 2), like watch's own interval check, and must never
// panic. The server is live so that, without the check, the initial poll
// succeeds and execution reaches the ticker exactly as the operator saw it;
// the request counter pins that a valid invocation is rejected before any
// work, not after a poll.
func TestTailRejectsNonPositiveIntervalInFollowMode(t *testing.T) {
	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Last-Modified-Version", "1")
		_, _ = io.WriteString(w, `[{"key":"X","version":1,"data":{"title":"hello"}}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	for _, arg := range []string{"0", "0s", "-5s"} {
		t.Run(arg, func(t *testing.T) {
			cmd := RootCmd()
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{"tail", "items", "--follow=true", "--interval=" + arg, "--db", filepath.Join(t.TempDir(), "tail.db")})

			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("tail items --follow=true --interval=%s panicked: %v", arg, r)
					}
				}()
				err = cmd.ExecuteContext(context.Background())
			}()

			if err == nil {
				t.Fatalf("tail items --interval=%s returned nil, want usage error", arg)
			}
			var cliErr *cliError
			if !errors.As(err, &cliErr) || cliErr.code != 2 {
				t.Fatalf("tail items --interval=%s error = %T %[2]v, want usageErr code 2", arg, err)
			}
			if !strings.Contains(err.Error(), "--interval must be positive") {
				t.Fatalf("tail items --interval=%s error = %q, want the positive-interval message", arg, err.Error())
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("requests = %d, want 0: a rejected interval must not poll", got)
			}
		})
	}
}

// --follow=false is the one-shot mode: it never builds a ticker and never
// reads the interval, so a zero interval is legitimate there and must poll
// once instead of being rejected or panicking. A positive interval takes the
// same path unchanged.
func TestTailOneShotIntervalDoesNotReachTicker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "1")
		_, _ = io.WriteString(w, `[{"key":"X","version":1,"data":{"title":"hello"}}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	for _, arg := range []string{"0", "-5s", "5ms"} {
		t.Run(arg, func(t *testing.T) {
			cmd := RootCmd()
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{"tail", "items", "--follow=false", "--interval=" + arg, "--db", filepath.Join(t.TempDir(), "tail.db")})

			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("tail items --follow=false --interval=%s panicked: %v", arg, r)
					}
				}()
				err = cmd.ExecuteContext(context.Background())
			}()
			if err != nil {
				t.Fatalf("tail items --follow=false --interval=%s: %v (stderr %q)", arg, err, errOut.String())
			}
			events := ndjsonEvents(t, out.String())
			if len(events) != 1 || events[0]["event"] != "upsert" || events[0]["key"] != "X" {
				t.Fatalf("events = %v, want a single upsert of X", events)
			}
		})
	}
}

// The banner has to describe the run the operator asked for. One-shot mode
// never builds a ticker and never reads --interval, so naming an interval and
// offering a Ctrl+C described a command that keeps polling and can be
// interrupted — neither of which is true of a command that has already
// returned. Reachable in this release now that --follow=false --interval 0 is
// accepted rather than rejected.
func TestTailOneShotBannerDescribesASinglePoll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "1")
		_, _ = io.WriteString(w, `[{"key":"X","version":1,"data":{"title":"hello"}}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	cmd := RootCmd()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"tail", "items", "--follow=false", "--interval=0", "--db", filepath.Join(t.TempDir(), "tail.db")})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("tail items --follow=false --interval=0: %v (stderr %q)", err, errOut.String())
	}
	banner := errOut.String()
	if !strings.Contains(banner, "Polling items once") {
		t.Fatalf("banner = %q, want it to say the command polls once", banner)
	}
	for _, unwanted := range []string{"every", "Ctrl+C", "0s"} {
		if strings.Contains(banner, unwanted) {
			t.Fatalf("banner = %q, must not mention %q: one-shot mode reads no interval and cannot be interrupted", banner, unwanted)
		}
	}

	// Follow mode is unchanged: it does poll on an interval and it does stop
	// on Ctrl+C, so its banner still says both.
	if got := tailStartBanner("items", 10*time.Second, true); got != "Tailing items every 10s (Ctrl+C to stop)" {
		t.Fatalf("follow banner = %q, want the unchanged wording", got)
	}
}

// An unreadable saved cursor must fail the poll before any fetch: without a
// trustworthy cursor there is no safe `since`, and treating the failure as a
// bootstrap at zero would skip deletions and checkpoint past them.
func TestEmitChanges_CursorReadErrorFailsBeforeFetch(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `[{"key":"A","version":7}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "tail.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}
	// Close the handle so the cursor SELECT fails while the seeded row stays
	// on disk; reopening later proves the cursor was never advanced.
	if err := db.Close(); err != nil {
		t.Fatalf("closing store: %v", err)
	}

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	var buf bytes.Buffer
	_, err = emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("emitChanges with unreadable cursor: want error, got nil")
	}
	if !strings.Contains(err.Error(), "reading cursor") {
		t.Errorf("error = %q, want it to name the cursor read", err.Error())
	}
	if hits != 0 {
		t.Errorf("server saw %d requests, want 0: nothing may be fetched without a cursor", hits)
	}
	if buf.Len() != 0 {
		t.Errorf("writer got %q, want empty when the cursor cannot be read", buf.String())
	}

	reopened, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if v, _, err := reopened.StoredLibraryVersion("tail:items"); err != nil || v != 6 {
		t.Errorf("cursor after failed read = %d, %v; want unchanged 6", v, err)
	}
}

// A page element that is not a well-formed identified object must fail the
// poll before delivery and checkpointing, so the faulty window is retried
// after the dependency is repaired instead of being skipped permanently.
func TestEmitChanges_MalformedObjectHoldsCursorForRetry(t *testing.T) {
	faulty := true
	var sinces []string
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		sinces = append(sinces, r.URL.Query().Get("since"))
		w.Header().Set("Last-Modified-Version", "7")
		if faulty {
			_, _ = io.WriteString(w, `[42,{"key":"A","version":7}]`)
			return
		}
		_, _ = io.WriteString(w, `[{"key":"A","version":7},{"key":"B","version":7}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		if faulty {
			t.Error("/deleted should not be fetched when a change object is malformed")
		}
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `{"items":[],"collections":[],"searches":[],"tags":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	_, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("emitChanges with scalar page element: want error, got nil")
	}
	if !strings.Contains(err.Error(), "decoding change object 0") {
		t.Errorf("error = %q, want it to name the malformed element", err.Error())
	}
	if buf.Len() != 0 {
		t.Errorf("writer got %q, want empty: the valid sibling must not deliver ahead of the faulty window", buf.String())
	}
	if v, _, err := db.StoredLibraryVersion("tail:items"); err != nil || v != 6 {
		t.Errorf("cursor after malformed object = %d, %v; want unchanged 6", v, err)
	}

	faulty = false
	buf.Reset()
	n, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err != nil {
		t.Fatalf("replay emitChanges: %v", err)
	}
	if n != 2 {
		t.Fatalf("replay emitted = %d, want 2", n)
	}
	if len(sinces) != 2 || sinces[0] != "6" || sinces[1] != "6" {
		t.Fatalf("poll since values = %q, want [6 6]: the faulty window was not retried", sinces)
	}
	events := ndjsonEvents(t, buf.String())
	if len(events) != 2 {
		t.Fatalf("replay delivered %d events, want 2", len(events))
	}
	if v, _, err := db.StoredLibraryVersion("tail:items"); err != nil || v != 7 {
		t.Errorf("cursor after replay = %d, %v; want 7", v, err)
	}
}

// An object without any resource identity must also fail the poll: emitting
// it with a null key and advancing would skip a window that can never be
// recovered.
func TestEmitChanges_IdentitylessObjectHoldsCursor(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `[{"version":7}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		t.Error("/deleted should not be fetched when a change object lacks identity")
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	_, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("emitChanges with identityless object: want error, got nil")
	}
	if !strings.Contains(err.Error(), "missing resource identity") {
		t.Errorf("error = %q, want it to name the missing identity", err.Error())
	}
	if v, _, err := db.StoredLibraryVersion("tail:items"); err != nil || v != 6 {
		t.Errorf("cursor after identityless object = %d, %v; want unchanged 6", v, err)
	}
}

// Tags identify by name rather than key, so the identity check must accept a
// well-formed tag object instead of rejecting the whole tags feed.
func TestEmitChanges_TagObjectDelivers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/tags", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "" {
			t.Errorf("since = %q, want empty on bootstrap", got)
		}
		w.Header().Set("Last-Modified-Version", "3")
		_, _ = io.WriteString(w, `[{"tag":"research","version":3}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)

	var buf bytes.Buffer
	n, err := emitChanges(context.Background(), c, db, "tags", "/tags", DeliverSink{Scheme: "stdout"}, &buf)
	if err != nil {
		t.Fatalf("tag page: %v", err)
	}
	if n != 1 {
		t.Fatalf("tag page emitted = %d, want 1", n)
	}
	events := ndjsonEvents(t, buf.String())
	if len(events) != 1 || events[0]["event"] != "upsert" {
		t.Fatalf("tag events = %q, want one upsert", buf.String())
	}
	if v, _, err := db.StoredLibraryVersion("tail:tags"); err != nil || v != 3 {
		t.Errorf("cursor after tag page = %d, %v; want 3", v, err)
	}
}
